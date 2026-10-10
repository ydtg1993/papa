package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// repeatMinTick 自动扫描的最短间隔：算出来的节拍会被钳到不小于它。
// 再密没有意义（每轮扫描本身要查两次库），而且会让 ticker 空转。写入侧（SetTaskRepeatInterval
// 与后台「设轮询周期」）也按它校验，所以不会出现"设了 3 秒却按 10 秒跑"的静默取整。
const repeatMinTick = 10 * time.Second

// ErrUnknownSite 按站点操作轮询队列时，站点名不认识（后台 `?site=` 传错 → 400）。
var ErrUnknownSite = errors.New("未知站点")

// repeatQueueState 一个站点（或默认 scope）的轮询队列运行态。
//
// **每站一份**：自己的锁（本站同一时刻只跑一轮，自动与手动不会互相插队）、自己的唤醒通道、
// 自己的累计计数。站点之间互不影响 —— A 站的积压不会拖住 B 站，A 站被停掉也不影响 B 站。
type repeatQueueState struct {
	mu       sync.Mutex    // 串行化本站的队列执行
	wake     chan struct{} // "数据变了，重算节拍"（容量 1，非阻塞发）
	repolled atomic.Int64  // 本站累计重投数（同时就是监控里那一行的"处理量"）
}

// repeatQueueKey 按站点的轮询队列名 —— 它同时是监控快照的 key（后台一行一个站）。
// 默认 scope（site == ""）保持历史名字 `repeat_queue`，命名站点是 `repeat_queue:<站点>`。
func repeatQueueKey(site string) string {
	if site == "" {
		return QueueRepeat
	}
	return QueueRepeat + ":" + site
}

// repeatQueueSite 从队列名反解站点。站点名必须是**登记过的** —— 否则后台会出现一行
// "看起来开着"的假队列，也破坏"不认识的名字一律当停用"这条约定（`queueEnabled` 的兜底）。
func (e *Engine) repeatQueueSite(name string) (string, bool) {
	if name == QueueRepeat {
		return "", true
	}
	site, found := strings.CutPrefix(name, QueueRepeat+":")
	if !found || site == "" {
		return "", false
	}
	if _, ok := e.Site(site); !ok {
		return "", false
	}
	return site, true
}

// repeatQueue 取某队列名对应的运行态（没登记返回 nil）。
func (e *Engine) repeatQueue(name string) *repeatQueueState { return e.repeatQueues[name] }

// repeatQueueSites 返回所有已登记的轮询队列站点：默认 scope 在最前，其余按站点名字典序。
// 顺序稳定，才能让日志、后台、以及"逐个站点跑"的结果可复现。
func (e *Engine) repeatQueueSites() []string {
	sites := make([]string, 0, len(e.repeatQueues))
	for name := range e.repeatQueues {
		if site, ok := e.repeatQueueSite(name); ok {
			sites = append(sites, site)
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i] == "" || sites[j] == "" {
			return sites[i] == "" // 默认 scope 永远排在最前
		}
		return sites[i] < sites[j]
	})
	return sites
}

// ensureRepeatQueue 预建某站点的队列运行态（启动期单线程调用）。
//
// **键必须在启动期备齐**：运行期再往 e.repeatQueues / e.queueRuns / e.queueCounters 里塞键，
// 会与 HTTP handler 里 `GetQueueStats` 的裸 range 撞成 `concurrent map read and map write`
// （进程级 fatal，recover 不了）。启动之后这几张 map 只读。
func (e *Engine) ensureRepeatQueue(site string) *repeatQueueState {
	name := repeatQueueKey(site)
	if st := e.repeatQueues[name]; st != nil {
		return st
	}
	st := &repeatQueueState{wake: make(chan struct{}, 1)}
	if e.repeatQueues == nil { // 手工构造的 Engine（测试/业务自己拼）没有这一步初始化
		e.repeatQueues = make(map[string]*repeatQueueState)
	}
	if e.queueRuns == nil {
		e.queueRuns = make(map[string]*queueRunState)
	}
	if e.queueCounters == nil {
		e.queueCounters = make(map[string]*atomic.Int64)
	}
	e.repeatQueues[name] = st
	if e.queueRuns[name] == nil {
		e.queueRuns[name] = &queueRunState{}
	}
	// 累计计数就用这一站的计数器本身，不再另立一份（两处维护同一个数就会漂）
	e.queueCounters[name] = &st.repolled
	return st
}

// ensureRepeatQueues 备齐所有站点的队列键。**默认 scope 永远建一份**：`crawler_tasks.site`
// 是后加的列，更早入库的 repeatable 行是空串、只在再次提交时才补上 —— 没有 `site = ”` 这个
// 队列，那些行就静默没人管了。
func (e *Engine) ensureRepeatQueues() {
	e.ensureRepeatQueue("")
	for key := range e.sites {
		e.ensureRepeatQueue(key)
	}
}

// siteAutoRepeat 本站要不要**自动**轮询（内存镜像，不查库：`queueEnabled` 在监控刷新路径上，
// 契约是"只读内存"）。默认 scope 恒 true —— 它没有站点声明，只能靠全局 `repeat_queue.enabled` 关。
// 站点没登记时按 true 处理：宁可多跑一轮，也别让一次登记遗漏把整站轮询静默停掉。
func (e *Engine) siteAutoRepeat(site string) bool {
	if site == "" {
		return true
	}
	s, ok := e.Site(site)
	return !ok || s.AutoRepeat
}

// repeatRepolledTotal 所有站点累计重投数之和（监控埋点 `repeat_repoll_total`）。
// 计数只有各站那一份（就是队列运行态里的计数器），这里现加 —— 不再另立一个总计数器，
// 两处维护同一个数就会漂。
func (e *Engine) repeatRepolledTotal() int64 {
	var total int64
	for _, st := range e.repeatQueues {
		total += st.repolled.Load()
	}
	return total
}

// repeatBaseQuery 两个轮询查询共用的底：本站、可轮询、已完成。
//
// `site = ?` **单独一个 Where**：到点条件里带 `OR`，而 gorm 只在"有多个表达式"时才给含
// AND/OR 的子句自动加括号 —— 拼进同一个字符串会把优先级搞乱。
func (e *Engine) repeatBaseQuery(site string) *gorm.DB {
	return e.db.Model(&models.CrawlerTask{}).
		Where("repeatable = ? AND status IN ?",
			models.RepeatableYes,
			[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}).
		Where("site = ?", site)
}

// repeatQueueQuery 本站「到点」的可轮询任务。
//
// 到点 = 未排期（NextRepeatAt 为 NULL：迁移前的老行、或从未被轮询过）或下次到点时间已过。
// 这是一条纯比较谓词，走 idx_repeat_due（repeatable, status, next_repeat_at）；周期本身在
// **写路径**换算成 NextRepeatAt（见 requeueRepeatTask 那条 UPDATE），读路径不做任何算术 ——
// 函数谓词会让索引失效，而且本仓没有真库测试，SQL 里的算术验不了。
//
// Model 不能省 —— 理由同 errorQueueQuery：`sampleQueueBacklogOne` 的 `Count(&n)`
// 推不出表名，没有 Model 就会静默把积压数永远留成 0。
func (e *Engine) repeatQueueQuery(site string) func() *gorm.DB {
	return func() *gorm.DB {
		return e.repeatBaseQuery(site).
			Where("next_repeat_at IS NULL OR next_repeat_at <= NOW()")
	}
}

// repeatForceQueueQuery 本站可轮询任务的**全量**查询（后台「轮询任务」总按钮用）：
// 与上面只差"不看周期"这一条。它比「立即执行」激进得多，所以是两个显式入口，不是同一个按钮。
func (e *Engine) repeatForceQueueQuery(site string) func() *gorm.DB {
	return func() *gorm.DB { return e.repeatBaseQuery(site) }
}

// soonestRepeatIn 返回本站"离最早一条到点还差多久"，用来让 ticker 跟上各自的周期。
// 负数 = 已经有到点的了（立刻扫）。
//
// 只看 NextRepeatAt（判据列）：
//   - 终态的行（会被真扫描）：已到点 / 未排期 → 0；否则它自己的到点时间。
//   - 跑着 / 排队中的行：只在**未来**才算数（它到点时多半已经跑完，扫描正好接上）；
//     已经过期的（任务跑得比自己的周期还久）直接排除 —— 它跑完之前投不出去，
//     让这种行参与只会把扫描钉在最短刻度上空转。
//
// 没有可轮询的行、或查库出错 → false，调用方退回全局节拍（DB 抖动不该让 ticker 崩，也不该刷日志）。
func (e *Engine) soonestRepeatIn(site string) (time.Duration, bool) {
	var row struct{ Epoch *int64 }
	err := e.repeatBaseQuery(site).
		Select(`MIN(CASE
			WHEN status IN ? THEN UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW()))
			WHEN next_repeat_at > NOW() THEN UNIX_TIMESTAMP(next_repeat_at)
			ELSE NULL END) AS epoch`,
			[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}).
		Scan(&row).Error
	if err != nil || row.Epoch == nil {
		return 0, false
	}
	return time.Until(time.Unix(*row.Epoch, 0)), true
}

// RepollRepeatableTasks 把**每个站点**各跑一遍（向后兼容业务 cron 里的既有用法）。
// 单站的耗时是 Σ 各站，站点多的时候用 RepollSiteRepeatableTasks 只跑要的那一个。
func (e *Engine) RepollRepeatableTasks() (int, error) {
	total := 0
	var errs []error
	for _, site := range e.repeatQueueSites() {
		n, err := e.RepollSiteRepeatableTasks(site)
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

// RepollSiteRepeatableTasks 重新投递**本站「到点」**的可轮询任务（success/failed），
// 返回实际投递数量。手动触发（后台「立即执行」/ `POST /api/repeatqueue/process`）走的就是它，
// 所以手动同样只扫到点的 —— 想把本站全投一遍（忽略周期），用 ForceRepollSiteRepeatableTasks。
//
// 只重投 success/failed，不碰还在 pending/processing 的——后者由 recover_queue 兜底，避免双入队。
// 分页流式 + 并发处理（复用 processInBatches），并发数与批大小取 repeat_queue 配置。
func (e *Engine) RepollSiteRepeatableTasks(site string) (int, error) {
	return e.repollRepeatQueue(site, false)
}

// ForceRepollSiteRepeatableTasks 把本站**所有可轮询的已完成任务**都投一遍 —— **忽略周期**。
// 后台那一站的「轮询任务」总按钮走它：适合"我刚改完站点参数，想立刻让这一站整体重跑一轮"。
//
// 它与到点那条走同一套重置语句，所以每条的"下次到点"会按各自周期往后推 —— 手动插一轮不会
// 把自动节奏打乱（下一跳该什么时候还是什么时候）。
func (e *Engine) ForceRepollSiteRepeatableTasks(site string) (int, error) {
	return e.repollRepeatQueue(site, true)
}

// repollRepeatQueue 跑一轮某站点的轮询队列（force = 忽略到点条件）。
func (e *Engine) repollRepeatQueue(site string, force bool) (int, error) {
	name := repeatQueueKey(site)
	st := e.repeatQueue(name)
	if st == nil {
		return 0, fmt.Errorf("%w: %q", ErrUnknownSite, site)
	}
	// 本站的锁：自动那一跳与后台手动点击不会互相插队（跨站点不互斥，各跑各的）
	st.mu.Lock()
	defer st.mu.Unlock()

	cfg := e.repeatQueueConfig()
	query := e.repeatQueueQuery(site)
	if force {
		query = e.repeatForceQueueQuery(site)
	}
	e.beginQueueRun(name)
	n, err := e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueRepeatTask(site))
	e.endQueueRun(name, n, err)
	e.sampleQueueBacklogOne(name, query)
	// 统计落到站点表 + 内存快照（库是落点，内存给监控页读）：本轮投递数、刚采到的积压、错误
	backlog := 0
	if s, ok := e.GetQueueStats()[name]; ok {
		backlog = s.Backlog
	}
	e.writeSiteRepeatStats(site, st.repolled.Load(), backlog, err)
	return n, err
}

// repeatResetScope 重投前把这一行重置为 pending 的条件：**此刻它还得属于本站、可轮询、且已完成**。
//
// 站点的判据是**队列的 site**（不是行上那个）—— 若行在批次 SELECT 与这次重置之间被改到了别的站，
// 守卫匹配不上（0 行）正好把它让给新站点的队列，而不是被本站再投一次。
//
// 批次 SELECT 与这次重置之间有个窗口，运营可能刚好点了后台的「停轮询」（或「重投」/「删除」
// 已经接手了这一行，甚至把它的 site 改到了别处）。这几列才是"它该不该被本站重投"的判据，
// 所以这里再确认一次 —— 停就是停，这一轮不再投它。
func repeatResetScope(db *gorm.DB, id uint, site string) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND site = ? AND repeatable = ? AND status IN ?", id, site, models.RepeatableYes,
			[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed})
}

// requeueRepeatTask 将单条 repeatable 任务重置为 pending 并重新投递。
func (e *Engine) requeueRepeatTask(site string) func(*models.CrawlerTask) bool {
	return func(t *models.CrawlerTask) bool {
		return e.requeueRepeatTaskIn(site, t)
	}
}

// requeueRepeatTaskIn 单条重投的实体（site 是**队列**的站点，见 repeatResetScope 的注释）。
func (e *Engine) requeueRepeatTaskIn(site string, t *models.CrawlerTask) bool {
	if e.stages[t.Stage] == nil {
		e.loggerSet.Engine.Warnf("repeat queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	// 条件重置（不用 task.UpdateStatus：它无条件、且被 worker 等多处共用，不往它上面加条件）。
	// 一条语句把三个字段一起写：状态、记录（上次轮询时刻）、判据（下次到点）。
	// 0 行 = 这一行已经不该被重投了：跳过，不计数、也不标 failed —— 它不是"重投失败"。
	res := repeatResetScope(e.db, uint(t.ID), site).Updates(map[string]any{
		"status":         models.TaskStatusPending,
		"last_repeat_at": gorm.Expr("NOW()"),
		// 下次到点 = 现在 + 自己的周期（没定就用全局）。换算只在这一条写路径上做一次，
		// 用 UNIX_TIMESTAMP 加法而不是 DATE_ADD(..., INTERVAL <表达式> SECOND)：后者把函数塞进
		// INTERVAL 的写法有语法/优化器上的余地，而本仓没有真库测试去验。时间一律取库里的 NOW()：
		// 写入端与判据端同一个时钟，不受应用与数据库时钟偏差影响。
		"next_repeat_at": gorm.Expr(
			"FROM_UNIXTIME(UNIX_TIMESTAMP(NOW()) + COALESCE(NULLIF(repeat_interval, 0), ?))",
			int64(e.repeatQueueConfig().Interval/time.Second)),
	})
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("repeat queue: reset task %d: %s", t.ID, res.Error.Error())
		return false
	}
	if res.RowsAffected == 0 {
		return false
	}
	// Repeatable 显式置真：本队列捞出来的行本来就该是可轮询的，写死比照抄行上的值更少一层怀疑。
	task := e.taskFromRecord(t)
	task.Repeatable = true
	if err := e.SubmitTask(task); err != nil {
		e.loggerSet.Engine.Errorf("repeat queue: submit task %d: %s", t.ID, err.Error())
		// SubmitTask 内部（submitToPool）已经失败标过 failed 了，这一句管的是它提前返回的那些路
		e.markRequeueFailed(QueueRepeat, t.ID, err)
		return false
	}
	if st := e.repeatQueue(repeatQueueKey(site)); st != nil {
		st.repolled.Add(1)
	}
	return true
}

// wakeRepeatQueue 让**某个站点**的轮询队列立刻重算扫描节拍：新提交了带周期的任务、后台改了某条
// 的周期、或把某条「开轮询」之后调它。节拍是 pull 出来的（只在 tick 到点或收到信号时重算），
// 不叫这一声就要等当前那次 sleep 到期 —— 可能是一整个全局 interval（比如 2 小时）。
//
// 只叫本站，**不扇出**：每次唤醒都会让被叫到的队列跑一条 `soonestRepeatIn`（整表 MIN 聚合），
// 而唤醒的触发源包括"批量提交带周期的任务"，扇出等于把这条查询放大成站点数倍。
//
// 非阻塞：它只是个"提醒"，丢了也无所谓（下一个 tick 照样会重算）。手搓的 Engine（测试里）
// 没建这个通道，nil 在 select 里永远走 default，安全。
func (e *Engine) wakeRepeatQueue(site string) {
	st := e.repeatQueue(repeatQueueKey(site))
	if st == nil {
		return
	}
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// wakeAllRepeatQueues 叫醒**所有**站点的轮询队列。给后台那两个人工动作（开/停轮询、设轮询周期）用：
// 它们手里没有行的 site（那两条路刻意不先查库，见 taskadmin 的注释），而人工点击的频率极低 ——
// 多叫几声只是让每个队列各重算一次节拍，代价可以忽略。
//
// 与 wakeRepeatQueue 的分工：**批量提交那条热路径必须精确叫**（一次批量提交可能几千条任务，
// 扇出等于把"每站一次整表 MIN 聚合"放大成站点数倍）。
func (e *Engine) wakeAllRepeatQueues() {
	for _, st := range e.repeatQueues {
		select {
		case st.wake <- struct{}{}:
		default:
		}
	}
}

// repeatTickInterval 本站下一次扫描的节拍：0 = 不自动轮询（总开关关着，仅手动触发）。
//
// 节拍 = min(全局 interval, 离本站最早一条到点还差多久)，并钳进 [repeatMinTick, 全局 interval]：
// 全局 interval 从"每条任务的周期"变成"最粗兜底" —— 任务自己定的 10 分钟就真是 10 分钟，
// 而新提交的行、被改过周期的行最多等这么久被发现（等不到时由 wakeRepeatQueue 叫醒）。
// 查不到数据（没有可轮询的行 / 库抖动）→ 退回全局节拍，DB 抖动不该让 ticker 崩。
//
// 这里**不看 `AutoRepeat`**：站点级开关是"要不要自动投"的闸门，放在 onTick 里判（见
// startRepeatQueue）—— 若把它算成 0，ticker 会停掉、只能等下一次唤醒，而一次库/声明的
// 抖动就可能让整站静默永久停轮询。
func (e *Engine) repeatTickInterval(site string) time.Duration {
	cfg := e.repeatQueueConfig()
	if !cfg.Enabled || cfg.Interval <= 0 {
		return 0
	}
	d, ok := e.soonestRepeatIn(site)
	if !ok {
		return cfg.Interval
	}
	if d < repeatMinTick {
		d = repeatMinTick
	}
	if d > cfg.Interval {
		return cfg.Interval
	}
	return d
}

// startRepeatQueues 每站起一个轮询 ticker（enabled/interval 运行期可热更）。
func (e *Engine) startRepeatQueues() {
	for _, site := range e.repeatQueueSites() {
		e.startRepeatQueue(site)
	}
}

// startRepeatQueue 启动某一站的轮询队列。
func (e *Engine) startRepeatQueue(site string) {
	st := e.repeatQueue(repeatQueueKey(site))
	if st == nil {
		return
	}
	e.runDynamicTicker(func() time.Duration { return e.repeatTickInterval(site) }, st.wake, func() {
		// 站点级闸门：`AutoRepeat: false` 的站只保留两个手动入口（后台「立即执行」与「轮询任务」），
		// 自动这一跳直接跳过。放在这里而不是把节拍算成 0 —— 闸门是"这一跳投不投"，
		// 不是"ticker 还活不活"。
		if !e.siteAutoRepeat(site) {
			return
		}
		if n, err := e.RepollSiteRepeatableTasks(site); err != nil {
			e.loggerSet.Engine.Errorf("repeat queue[%s]: auto repoll: %s", repeatQueueKey(site), err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("repeat queue[%s]: auto repolled %d tasks", repeatQueueKey(site), n)
		}
	})
}
