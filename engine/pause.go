package engine

import (
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// 本文件是**暂停**的唯一定义：一处状态、两个消费者。
//
// 起因：暂停原来长在熔断器里（`internal/breaker` 的 `paused` + `resumeCh`），于是"人工节流"
// 与"自动止损"共用一份状态 —— 两处的理由不一样（一个看执行结果、一个看人的意图），
// 显示上已经开始互相打架（后台手动暂停却显示"窗口内 0 次失败、阈值 0"）。
//
// 现在的分工：
//   - **状态只在这里**：`paused` 表，按 scope（整站 / 某站的某阶段）记 reason / source / 触发快照；
//   - **取任务点**（`pauseGate`，实现 workerpool.Gate）：冻住 —— 队列里那批也不动；
//   - **入队点**（`submitToStage`）：收口 —— 不接新活，寄放到溢出表，恢复后回灌；
//   - **熔断器**退化成"自动往这张表里写一条的人"（`Source=breaker`），它只判据 + 报警。
//
// 往这张表里写的人有三个：后台/业务手动（`PauseSite` / `PauseStage` / `PauseCrawling`）、
// 熔断自动触发、以及**启动那一刻的 `SiteSpec.StartPaused`**（`applyStartPaused`，声明期的一次性
// 整站闸门，走的还是 `PauseSite` 那条路）。

// pauseSource 这条暂停是谁下的。后台要显示，也是"熔断触发"与"人工暂停"的唯一区别依据 ——
// 别再用"enabled && stage"那种启发式去猜（熔断关着时的手动暂停、以及阶段级的手动暂停都会猜错）。
const (
	PauseSourceManual  = "manual"
	PauseSourceBreaker = "breaker"
)

// pauseScope 一条暂停管谁。**Stage 为空 = 整站**（阶段名不会是空的，天然不冲突）。
//
// Site **永远精确匹配**，不做通配：更早入库的行 `site` 是空串（那是"默认 scope"时代的遗留），
// 它不等于"所有站点" —— 空站点当成通配是这里最容易写错、也最危险的一处。
type pauseScope struct {
	Site  string
	Stage string
}

// pauseState 一条暂停的现场。`At` 是下这条暂停的时刻，**不随后续变化改写**
// （排查时要看的是"当时为什么停的"，与熔断那套触发快照同一取向）。
type pauseState struct {
	Reason string // 给人看的："后台手动暂停" / "窗口内终态失败数达到阈值"
	Source string // PauseSourceManual / PauseSourceBreaker
	At     time.Time
	Stage  string // 熔断触发时是哪个阶段在失败；手动暂停为空
	// Failures 触发那一刻窗口内的失败数（手动暂停取当时的 Sum()，与今天一致 ——
	// 它是后台 payload 里的一个字段，别顺手改成 0）。
	Failures int
}

/* ---------- 查询（三种，别混用） ---------- */

// isPaused 这个 (站点, 阶段) 现在该不该停：该站的整站暂停 ∥ 它自己那条。
// **闸门与提交点用这一个**（答案要精确到 scope）。
func (e *Engine) isPaused(scope pauseScope) bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	return e.isPausedLocked(scope)
}

// pauseStateOfScope 这个 scope 现在停不停、因为什么 —— **含继承**：整站那条涵盖它的每个阶段；
// 阶段自己那条（更具体）优先。后台的阶段行与闸门用的是同一套判定，不会一处说停、一处说跑。
func (e *Engine) pauseStateOfScope(scope pauseScope) (pauseState, bool) {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	if st, ok := e.paused[scope]; ok {
		return st, true
	}
	if scope.Stage != "" {
		if st, ok := e.paused[pauseScope{Site: scope.Site}]; ok {
			return st, true
		}
	}
	return pauseState{}, false
}

// pauseInfo 后台要的一条站点暂停快照：命中的是哪一条（scope）、现场、以及该站最近一次放行。
type pauseInfo struct {
	Scope     pauseScope // 命中的 scope：Stage 为空 = 那个站的整站那一条
	State     pauseState
	Paused    bool
	ResumedAt time.Time
}

// pauseInfoOf 该站现在停不停、停在哪个 scope、现场是什么。后台的横幅、站点卡片与阶段行都读它。
//
// 命中的**优先整站那条**（它涵盖得更广）；只有阶段级条目时挑**最早**的那条 ——
// 不能顺手返回遍历到的第一条：map 顺序随机，同一个库、同一份声明，后台 payload 会一会儿一变。
func (e *Engine) pauseInfoOf(site string) pauseInfo {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	info := pauseInfo{ResumedAt: e.resumedAtOfSiteLocked(site)}
	if st, ok := e.paused[pauseScope{Site: site}]; ok {
		info.Scope, info.State, info.Paused = pauseScope{Site: site}, st, true
		return info
	}
	for sc, st := range e.paused {
		if sc.Site != site {
			continue
		}
		if !info.Paused || st.At.Before(info.State.At) {
			info.Scope, info.State, info.Paused = sc, st, true
		}
	}
	return info
}

// resumedAtOfSiteLocked 该站最近一次被放行的时刻；调用方须已持 pauseMu。
func (e *Engine) resumedAtOfSiteLocked(site string) time.Time {
	var latest time.Time
	for sc, at := range e.lastResumed {
		if sc.Site == site && at.After(latest) {
			latest = at
		}
	}
	return latest
}

// isPausedLocked 判定本体；调用方须已持 pauseMu。
func (e *Engine) isPausedLocked(scope pauseScope) bool {
	if len(e.paused) == 0 {
		return false
	}
	if _, ok := e.paused[pauseScope{Site: scope.Site}]; ok {
		return true // 整站暂停涵盖该站所有阶段
	}
	if scope.Stage == "" {
		return false // 问的就是整站，上面那条已经答了
	}
	_, ok := e.paused[scope]
	return ok
}

/* ---------- 变更 ---------- */

// setPause 下一条暂停；这个 scope 已经停着就返回 false（**且不覆盖已有的 reason/快照** ——
// 后台"空点不记审计"那条规则、以及"人工暂停不该被熔断触发改写成熔断"都靠这个）。
//
// 判定按**精确 scope**（不看继承）：所以某站只被手动停了 `catalog` 时，**熔断触发照样会追加一条
// 整站暂停** —— 那是熔断的本职（它管的是"这一站现在不能抓了"，比某个阶段的意图更宽），
// 而反过来（整站停着时又来了阶段级的暂停/熔断）会被这条去重挡掉。
func (e *Engine) setPause(scope pauseScope, st pauseState) bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	if e.isPausedLocked(scope) {
		return false
	}
	if st.At.IsZero() {
		st.At = time.Now()
	}
	if e.paused == nil {
		e.paused = make(map[pauseScope]pauseState)
	}
	e.paused[scope] = st
	e.broadcastPauseLocked()
	return true
}

// clearScope 放行一个 scope（只清它自己那条）；本来就没事返回 false。
func (e *Engine) clearScope(scope pauseScope) bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	if _, ok := e.paused[scope]; !ok {
		return false
	}
	delete(e.paused, scope)
	e.recordResumeLocked(scope)
	e.broadcastPauseLocked()
	return true
}

// clearSite 放行整站：**该站的所有条目一起清**（整站那条 + 各阶段那些）。
//
// 为什么不分那么细：后台对一站只有一个「恢复」按钮。只清整站那条的话，该站还留着阶段条目 ——
// 横幅还是红的、再点一次却回"当前不在暂停态"，界面上就成了"卡住且没出路"
// （`engine/sitestat.go` 里那个残留 true 的坑就是同一类）。
func (e *Engine) clearSite(site string) bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	cleared := false
	for sc := range e.paused {
		if sc.Site == site {
			delete(e.paused, sc)
			e.recordResumeLocked(sc)
			cleared = true
		}
	}
	if cleared {
		e.broadcastPauseLocked()
	}
	return cleared
}

// clearAllPauses 放行所有站点（后台横幅上那个「全部恢复」）。
func (e *Engine) clearAllPauses() bool {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	if len(e.paused) == 0 {
		return false
	}
	for sc := range e.paused {
		delete(e.paused, sc)
		e.recordResumeLocked(sc)
	}
	e.broadcastPauseLocked()
	return true
}

// recordResumeLocked 记下"这个 scope 被放行了"；调用方须已持 pauseMu。
func (e *Engine) recordResumeLocked(scope pauseScope) {
	if e.lastResumed == nil {
		e.lastResumed = make(map[pauseScope]time.Time)
	}
	e.lastResumed[scope] = time.Now()
}

// broadcastPauseLocked 叫醒所有等着的人：**换一个新 channel 并 close 旧的**。
// 关掉的那个不可能再被复用，所以"暂停 → 恢复 → 再暂停"不会出现"恢复信号早就关着了、
// 第二次暂停立刻被放行"。调用方须已持 pauseMu。
func (e *Engine) broadcastPauseLocked() {
	if e.pauseChanged == nil {
		e.pauseChanged = make(chan struct{})
	}
	close(e.pauseChanged)
	e.pauseChanged = make(chan struct{})
}

// wakeDrainNow 叫醒回灌循环（恢复之后立刻把寄放的任务灌回去，别等下一个 tick）。
// 非阻塞：容量 1 的通道 + default 分支，重复叫醒只留一次（与 wakeRepeatQueue / delayCh 同一写法）。
func (e *Engine) wakeDrainNow() {
	select {
	case e.wakeDrain <- struct{}{}:
	default:
	}
}

/* ---------- 取任务点：闸门 ---------- */

// pauseGate 一个池子问"我这个 scope 该不该停"的入口。实现 workerpool.Gate。
//
// **每个池一份、各自 scope**（`buildPool` 按自己的 site+stage 建）：于是"整站暂停"与
// "某个阶段暂停"都由同一个适配器回答，不需要在池子上串两道闸。
type pauseGate struct {
	e     *Engine
	scope pauseScope
}

// pauseGateFor 造一份闸门。**纯值构造，不碰任何锁** —— `buildPool` 跑在活池登记表的写锁里
// （`pools.go` 持锁调 `create()`），这里若取 pauseMu 就是 `pools.mu ↔ pauseMu` 死锁。
func (e *Engine) pauseGateFor(scope pauseScope) workerpool.Gate {
	return pauseGate{e: e, scope: scope}
}

// Wait 暂停中就一直等，直到放行或停机；返回 true = 继续干活，false = 该停机了。
//
// 循环 + 每轮重读是本实现的要点：广播是**全局**的（任何一个 scope 变了都叫醒所有人），
// 醒来必须重新判定自己这一条。**不能在循环外缓存 channel** —— 关掉之后那个 channel 永远就绪，
// 在"别的站变了、我这个 scope 还停着"这种最常见的组合下会变成 100% CPU 的热自旋。
func (g pauseGate) Wait(stop <-chan struct{}) bool {
	for {
		g.e.pauseMu.Lock()
		paused := g.e.isPausedLocked(g.scope)
		ch := g.e.pauseChanged
		g.e.pauseMu.Unlock()
		if !paused {
			return true
		}
		if ch == nil { // 手工拼的 Engine（测试）没有广播通道：没有变更可言，直接按停机处理
			return false
		}
		select {
		case <-ch: // 状态变了：重查（可能变的不是我这个 scope）
		case <-stop:
			return false
		}
	}
}

/* ---------- 后台要的那些 ---------- */

// PauseStage 暂停**某站的某个阶段**（引擎 API；后台接它要等下一步）。
// 站点不认识返回 false。
func (e *Engine) PauseStage(site, stage, reason string) bool {
	if !e.knownSite(site) || stage == "" {
		return false
	}
	return e.setPause(pauseScope{Site: site, Stage: stage}, pauseState{
		Reason: reason, Source: PauseSourceManual,
	})
}

// ResumeStage 放行某站的某个阶段（只清它自己那条）。本来就没事返回 false。
func (e *Engine) ResumeStage(site, stage string) bool {
	if !e.knownSite(site) || stage == "" {
		return false
	}
	return e.clearScope(pauseScope{Site: site, Stage: stage})
}

// knownSite 引擎认不认识这个站点。判据是**它有没有一份 per-site 抓取配置（熔断器）**：
// `SetSiteBreaker` 是导出 API，使用者会只调它、不调 `SetSite`；而 `siteBreaker` 正好就是
// "有哪些站配了抓取策略"—— 那才是"暂停这个站的抓取"该管的那批。
// 用 `e.sites` 的话，`PauseCrawling` 会静默漏掉这类站点（"全部暂停"变成半停）。
func (e *Engine) knownSite(site string) bool {
	if site == "" {
		return false
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	return e.siteBreaker[site] != nil
}

/* ---------- 启动那一刻：声明里的 StartPaused ---------- */

// applyStartPaused 把声明了 `StartPaused` 的站点闸住（`ApplyRegisterStage` 在**投入口任务之前**
// 调一次，顺序的理由见那里）。
//
// 为什么要有它：`StageSpec.Entry` 只说"这个阶段有入口任务"，管不了"现在跑不跑"。
// "这一站启动先别动"是**整站**级别的事 —— 而那正是暂停信号（`SiteSpec.StartPaused` 压进
// core.Site、落到这里）。原先由一个三态枚举（auto/manual/off）兼职表达，结果是 manual 只管住
// 了入口那一投：启动恢复、轮询队列照样把这一站跑起来，看着像"声明没生效"。
//
// 闸住之后这一站**什么都没发生**：入队点不再建池、任务一律折回溢出表（`queue_spill_backlog` 会涨），
// 启动恢复与轮询队列捞回来的任务也一样；后台点「恢复整站」（`ResumeSite`）时 `wakeDrainNow`
// 立刻把它们灌回。
//
// 走 `PauseSite` 而不是自己 `setPause`：来源是 manual、现场带当时的失败数、并把暂停状态写进
// 站点快照（后台站点表的「已暂停」列）—— 与人在后台点一下完全同一条路，没有第二套语义。
// 暂停不跨重启，所以"每次启动都重新闸上、运行期放的闸不会被声明改回去"是这句话的自然结果。
func (e *Engine) applyStartPaused() {
	for _, site := range e.startPausedSites() {
		if !e.PauseSite(site, "站点声明 StartPaused") {
			// 静默失效是最难查的那类问题：声明写了、日志里一个字都没有、任务却照跑。
			e.loggerSet.Engine.Warnf("站点 %s: 声明了 StartPaused，但这次没能闸住（它没有配抓取策略？）——"+
				"启动后会照常跑，去后台「站点控制」卡上手动暂停", site)
			continue
		}
		e.loggerSet.Engine.Infof("站点 %s: 声明了 StartPaused —— 启动即闸住整站；"+
			"入口任务照投（先寄放在溢出表、池子不建），去后台「站点控制」卡点「恢复整站」放开", site)
	}
}

// startPausedSites 声明了 StartPaused 的站点键，**按名字典序**（读的是声明快照，不是熔断器那张表：
// 这里问的是"声明怎么说"，不是"配了抓取策略的有哪些"）。
func (e *Engine) startPausedSites() []string {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	out := make([]string, 0, len(e.sites))
	for key, s := range e.sites {
		if s.StartPaused {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// allSites 有一份 per-site 抓取配置的站点键（按名字典序）—— 与 BreakerStatuses 同一批。
func (e *Engine) allSites() []string {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	out := make([]string, 0, len(e.siteBreaker))
	for key := range e.siteBreaker {
		out = append(out, key)
	}
	// 不在这里排：调用方（PauseCrawling / BreakerStatuses）自己排，免得同一个列表两种顺序。
	return out
}

// pausedSites 现在有暂停条目的站点键（去重快照）。ResumeCrawling 用它先记下「哪些站停着」，
// clearAllPauses 之后逐站把站点快照翻回未暂停 —— 落库必须发生在放锁之后（见 ResumeSite 的注释）。
func (e *Engine) pausedSites() []string {
	e.pauseMu.Lock()
	defer e.pauseMu.Unlock()
	seen := make(map[string]bool)
	for sc := range e.paused {
		seen[sc.Site] = true
	}
	out := make([]string, 0, len(seen))
	for site := range seen {
		out = append(out, site)
	}
	return out
}

// 编译期确认这个适配器满足池子那侧的接口（那边只声明了自己要的最小接口，不认识本包）。
var _ workerpool.Gate = pauseGate{}
