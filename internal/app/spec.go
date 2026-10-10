package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/internal/breaker"
)

// 本文件是**阶段与站点声明**：各站一份文件（脚手架生成的 `configs/sites/<站名>.go`），
// main.go 只要一行 `app.RegisterSites(configs.Sites()...)`。
//
// 为什么参数不放 config.yaml：一个阶段的存在本来就离不开代码（必须有个 fetcher），
// 参数再放另一份文件，等于加一个阶段要改两处、想看全貌要翻两个地方。现在一处写全 ——
// 并发、队列、间隔、重试、要不要投入口、属于哪个站、熔断阈值。
//
// config.yaml 里**不再有 `crawler.stages` 段**：阶段的存在性与参数都以这份声明为准
//（`crawler.breaker` 仍是默认 scope 的熔断阈值，可被 SiteSpec.Breaker 按站点覆盖）。

// SiteSpec 一个站点（或一个分组）的声明。多站就是多个 SiteSpec，各自的阶段名要能区分
// （约定带站点前缀，如 `hgd_catalog` / `siteb_catalog` —— 重名会直接报错并提示）。
type SiteSpec struct {
	// Key 站点键：熔断 scope、日志与监控维度、落库的 `crawler_tasks.site`。
	// 留空表示"未归属"（落在默认 scope，用 crawler.breaker 那把闸门）。
	Key string
	// BaseURL 站点根地址。框架不拿它当限制，只是方便 handler 取用
	//（`engine.Site(task.Site)` → BaseURL）。
	BaseURL string
	// Headers 本站的请求头（UA / Referer / Cookie / Accept-Language…）：这个站的任务抓任何页面
	// 都自动带上（静态抓取与浏览器渲染都认），**同键覆盖** `html.headers` / `browser.headers`；
	// 值为空串 = 删掉那个头。handler 里还能对单次请求 `papa.WithHeaders(ctx, …)` 再覆盖。
	// 多站同进程要"各站一套 UA/Cookie"就靠它 —— 也是把请求头这条"全局项"从 §4.1 那张表里拿掉的那一步。
	Headers map[string]string
	// RestrictedKeywords 本站自己的"受限页"文案，追加到 `htmlfetch.RestrictedReason` 的默认词表之后
	//（如"安全验证""请稍后再试"这类本站特有的话术）。命中后一般返回 `papa.RestrictedPageError`。
	RestrictedKeywords []string
	// Breaker 本站的熔断阈值；nil = 用 crawler.breaker 那份默认。
	Breaker *BreakerSpec
	// AutoRepeat 本站要不要**自动**周期轮询：**不写 = 自动**（与框架一直以来的行为一致），
	// 显式写 `&false` = 本站只在后台手动触发（轮询队列那一行显示"已停用"，但两个手动入口都在）。
	// 用指针而不是 bool，就是为了区分"没写"和"写了 false" —— 后者才是"我确实要关掉它"。
	AutoRepeat *bool
	// ErrorQueue / RecoverQueue / RepeatQueue 本站三个治理队列。**三条都得声明**：这一站不跑哪条，
	// 就写 `Enabled: &false` 一行（见各自的 Spec 类型）。参数只在声明里 —— config.yaml 里没有这三段，
	// 也没有"跟全局那份"这一层了；开了就得写全，缺一项 / 越界在 RegisterSites 时 panic 点名。
	// 注意：repeat 的"要不要**自动**轮询"是上面的 AutoRepeat（运营层，库为事实），与这里的
	// RepeatQueue.Enabled（声明层：这一站有没有这条队列）是「与」关系。
	ErrorQueue   *ErrorQueueSpec
	RecoverQueue *RecoverQueueSpec
	RepeatQueue  *RepeatQueueSpec
	Stages       []StageSpec
}

// 三个治理队列的声明。**三条都得写**（SiteSpec.ErrorQueue / RecoverQueue / RepeatQueue 为 nil
// 就是漏了，RegisterSites 直接报并点名）；每条队列的字段也都得写全 —— 开了缺一项、值越界，同样启动就炸。
//
// 与 crawler.breaker 同一套取向（`enabled=true` 时 threshold 必填且 > 0）：宁可启动报，也别让写错的
// 配置静默变成另一种行为。**关掉只写一行**：`Enabled: &false` —— 关着的队列不看参数（同 breaker）。
//
// 各自只有自己用得到的字段：写错字段是**编译期**错误（不再有"共用一个类型 + 用不上的键运行期报错"那套）。
// 改这些参数要**重启**：声明是启动时读一次（没有后台热更那回事了）。

// ErrorQueueSpec 本站错误队列：把 failed 的任务重投回各自的阶段（自动轮询 + 后台手动触发）。
type ErrorQueueSpec struct {
	// Enabled 本站要不要这条队列。**必须显式写**（nil = 漏写 → 启动报错）：用指针就是为了区分
	// "没写"与"写了 false" —— 后者才是"我确实不跑它"。
	Enabled *bool
	// WorkerCount 并发重新投递的数量。开了必填且 > 0（为 0 时池子里没有 worker，任务会静静躺在队列里）。
	WorkerCount int
	// Interval 自动轮询间隔，如 "4h"（开了必填）；**"0" = 不自动轮询**，只留后台手动触发。
	Interval string
	// MaxRetry 单条任务最多再处理几代；**0 = 不限**（这是合法取值，不是"跟默认"）。
	MaxRetry int
	// BatchSize 每批查询处理的任务数，开了必填且 > 0（分页流式，别把海量失败任务一次性全量加载）。
	BatchSize int
}

// RecoverQueueSpec 本站启动恢复：进程启动时把「未到终态」（pending/processing）的任务重新入队，
// **只在启动跑一次**（没有周期、没有手动入口，见 docs/RECOVER_QUEUE.md）。
type RecoverQueueSpec struct {
	// Enabled 本站要不要启动恢复。**必须显式写**；&false = 这一站重启后不捡回中断的任务。
	Enabled *bool
	// WorkerCount 并发重新入队的数量，开了必填且 > 0。
	WorkerCount int
	// BatchSize 每批查询处理的任务数，开了必填且 > 0。
	BatchSize int
}

// RepeatQueueSpec 本站周期轮询队列：把**到点的** repeatable 任务重投回各自阶段。
//
// 周期是**每条任务自己的**（`Task.RepeatInterval`，0 = 用本站的 Interval）；这里的 Interval 是
// "最粗兜底"的扫描间隔。本站要不要**自动**跑还有一道运营层的闸门（SiteSpec.AutoRepeat，库为事实）。
type RepeatQueueSpec struct {
	// Enabled 本站要不要这条队列。**必须显式写**；&false = 这一站没有轮询队列。
	Enabled *bool
	// WorkerCount 并发重新投递的数量，开了必填且 > 0。
	WorkerCount int
	// Interval 最粗兜底的扫描间隔，开了必填且 **> 0**（ticker 会自动提前到最早一条到点）。
	// "不自动跑"用 SiteSpec.AutoRepeat 关，别在这里写 "0"。
	Interval string
	// BatchSize 每批查询处理的任务数，开了必填且 > 0。
	BatchSize int
}

// BreakerSpec 一个站点的熔断阈值（覆盖 crawler.breaker 的默认值）。
type BreakerSpec struct {
	// Enabled 本站是否启用熔断。**注意它不看默认值** —— 写了这个字段就由它说了算，
	// 于是"全局开着、某个站关掉"和"全局关着、某个站单独开"都能表达。
	Enabled   bool
	Threshold int    // 窗口内终态失败数达到它即暂停；Enabled 时必须 > 0（0 = 用默认值）
	Window    string // 统计窗口，如 "5m"；空 = 用默认值
}

// StageSpec 一个阶段的完整声明。
type StageSpec struct {
	// Fetcher 阶段的实现。名字取自它自己的 `GetStage()`，不在这里写第二遍。
	// 实现了 engine.EntrySubmitter（`SubmitEntries`）的阶段，可以在启动时投一批入口任务。
	Fetcher     engine.Fetcher
	WorkerCount int
	QueueSize   int
	// Delay 任务间隔："5m"（固定）或 "10s-30s"（随机区间）；空 = 不延迟。
	Delay string
	Retry RetrySpec
	// AutoStart 启动时投本阶段的入口任务（fetcher 实现 SubmitEntries 才有得投）。
	// 显式写出来，"是关的、不是漏的"启动时会打一条 Info。
	AutoStart bool
}

// RetrySpec 重试声明。
type RetrySpec struct {
	MaxAttempts int    // 含首次执行；<=0 用默认 3
	Backoff     string // 退避基数（指数递增），如 "30s"；空 = 1s
}

// RegisterSites 注册站点与阶段。声明有问题**启动即失败**（与配置校验层同一个取向：
// 该在启动时炸掉的东西，别等跑到某条任务上才变成另一种行为）。
func (a *App) RegisterSites(sites ...SiteSpec) {
	if err := validateSites(sites); err != nil {
		panic(err)
	}
	owner := make(map[string]string, len(sites)) // 阶段名 → 已声明的站点，用来查重
	for i, spec := range sites {
		site := spec.snapshot() // 声明 → 框架内部流通的快照，一处转换
		if site.Key != "" {
			a.sites[site.Key] = site
			a.Engine.SetSite(site)
		}
		if spec.Breaker != nil {
			cfg, err := spec.Breaker.toConfig(a.Config.Crawler.Breaker)
			if err != nil {
				panic(fmt.Errorf("站点 %q 的熔断配置: %w", site.Key, err))
			}
			a.Engine.SetSiteBreaker(site.Key, cfg)
		}
		// 三个治理队列按站点声明（没有"全局那份"了）。**每条站点声明都得写全**（见那三个 Spec 类型）。
		// 未归属（Key 为空）的声明没有站点行，三队列对它一律不跑 —— 写了就报，别静默忽略。
		if site.Key == "" {
			if spec.ErrorQueue != nil || spec.RecoverQueue != nil || spec.RepeatQueue != nil {
				panic(fmt.Errorf("未归属（Key 为空）的站点声明不能配队列参数：三队列都按站点跑，" +
					"未归属的任务没有队列 —— 要给它们治理，先给这一站一个 Key"))
			}
		} else {
			errQ, err := spec.errorQueueConfig()
			if err != nil {
				panic(fmt.Errorf("站点 %q 的错误队列: %w", site.Key, err))
			}
			recQ, err := spec.recoverQueueConfig()
			if err != nil {
				panic(fmt.Errorf("站点 %q 的启动恢复: %w", site.Key, err))
			}
			repQ, err := spec.repeatQueueConfig()
			if err != nil {
				panic(fmt.Errorf("站点 %q 的轮询队列: %w", site.Key, err))
			}
			a.Engine.SetSiteQueues(site.Key, engine.SiteQueues{Error: errQ, Recover: recQ, Repeat: repQ})
		}
		for j, st := range spec.Stages {
			plan, err := planStage(site, st, owner)
			if err != nil {
				panic(fmt.Errorf("%s 的第 %d 个阶段: %w", siteLabel(site, i), j+1, err))
			}
			a.applyStagePlan(plan)
		}
	}
}

// snapshot 把站点声明压成框架内部流通的快照（core.Site）：引擎侧、告警、入口回调都读它。
// **加字段就改这里**，别在 RegisterSites 里另抄一份 —— 抄的那份不会跟着声明走。
func (s SiteSpec) snapshot() core.Site {
	return core.Site{
		Key:                s.Key,
		BaseURL:            s.BaseURL,
		Headers:            s.Headers,
		RestrictedKeywords: s.RestrictedKeywords,
		// 指针只活在声明层：这里就把三态压成一个 bool（不写 = 自动）
		AutoRepeat: s.AutoRepeat == nil || *s.AutoRepeat,
	}
}

// errQueueNotDeclared 三条队列漏声明时的报错（点名缺的是哪一条）。
func errQueueNotDeclared(field string) error {
	return fmt.Errorf("%s 没有声明 —— 三个治理队列都要写（ErrorQueue / RecoverQueue / RepeatQueue）；"+
		"这一站不跑它就写 `%s: &papa.%sSpec{Enabled: &off}`", field, field, field)
}

// errQueueEnabledMissing Enabled 漏写时的报错：必须显式写 true / false —— 用指针就是为了让"没写"
// 与"写了 false"分得开，漏写不能默认成任何一种。
func errQueueEnabledMissing(field string) error {
	return fmt.Errorf("%s.Enabled 必须显式写（true / false）", field)
}

// errorQueueConfig 把站点声明解析成引擎侧的生效值。声明不合法就报错（由 RegisterSites panic）。
//
// **关着的队列不看参数**（`Enabled: &false` 只写一行就够），与 crawler.breaker 同一档：
// 开了才校验，缺一项 / 越界都当场报。
func (s SiteSpec) errorQueueConfig() (config.ErrorQueueConfig, error) {
	q := s.ErrorQueue
	if q == nil {
		return config.ErrorQueueConfig{}, errQueueNotDeclared("ErrorQueue")
	}
	if q.Enabled == nil {
		return config.ErrorQueueConfig{}, errQueueEnabledMissing("ErrorQueue")
	}
	if !*q.Enabled {
		return config.ErrorQueueConfig{}, nil
	}
	if q.WorkerCount <= 0 {
		return config.ErrorQueueConfig{}, fmt.Errorf("ErrorQueue.WorkerCount 必须 > 0" +
			"（为 0 时池子里没有 worker，失败任务会静静躺在队列里）")
	}
	if q.BatchSize <= 0 {
		return config.ErrorQueueConfig{}, fmt.Errorf("ErrorQueue.BatchSize 必须 > 0（每批处理多少条；0 = 一条都不处理）")
	}
	if q.MaxRetry < 0 {
		return config.ErrorQueueConfig{}, fmt.Errorf("ErrorQueue.MaxRetry 不能为负（0 = 不限代数）")
	}
	if q.Interval == "" {
		return config.ErrorQueueConfig{}, fmt.Errorf(`ErrorQueue.Interval 必填："4h" 这种自动轮询间隔；` +
			`"0" = 不自动轮询，仅后台手动触发`)
	}
	interval, err := time.ParseDuration(q.Interval)
	if err != nil {
		return config.ErrorQueueConfig{}, fmt.Errorf("ErrorQueue.Interval 无法解析: %w", err)
	}
	if interval < 0 {
		return config.ErrorQueueConfig{}, fmt.Errorf(`ErrorQueue.Interval 不能为负（"0" = 不自动轮询，仅后台手动触发）`)
	}
	return config.ErrorQueueConfig{
		Enabled:     true,
		WorkerCount: q.WorkerCount,
		Interval:    interval,
		MaxRetry:    q.MaxRetry,
		BatchSize:   q.BatchSize,
	}, nil
}

// recoverQueueConfig 同上；启动恢复没有 interval / max_retry 这两项 —— 类型上就不存在（编译期挡掉）。
func (s SiteSpec) recoverQueueConfig() (config.RecoverQueueConfig, error) {
	q := s.RecoverQueue
	if q == nil {
		return config.RecoverQueueConfig{}, errQueueNotDeclared("RecoverQueue")
	}
	if q.Enabled == nil {
		return config.RecoverQueueConfig{}, errQueueEnabledMissing("RecoverQueue")
	}
	if !*q.Enabled {
		return config.RecoverQueueConfig{}, nil
	}
	if q.WorkerCount <= 0 {
		return config.RecoverQueueConfig{}, fmt.Errorf("RecoverQueue.WorkerCount 必须 > 0" +
			"（为 0 时池子里没有 worker，恢复出来的任务会静静躺在队列里）")
	}
	if q.BatchSize <= 0 {
		return config.RecoverQueueConfig{}, fmt.Errorf("RecoverQueue.BatchSize 必须 > 0（每批处理多少条；0 = 一条都不处理）")
	}
	return config.RecoverQueueConfig{
		Enabled:     true,
		WorkerCount: q.WorkerCount,
		BatchSize:   q.BatchSize,
	}, nil
}

// repeatQueueConfig 同上。Interval 必须 > 0：这里是"最粗兜底"的扫描节拍，"不自动跑"是
// SiteSpec.AutoRepeat（运营层闸门，见 AutoRepeat 的注释）—— 同一件事不在这里再开一个开关。
func (s SiteSpec) repeatQueueConfig() (config.RepeatQueueConfig, error) {
	q := s.RepeatQueue
	if q == nil {
		return config.RepeatQueueConfig{}, errQueueNotDeclared("RepeatQueue")
	}
	if q.Enabled == nil {
		return config.RepeatQueueConfig{}, errQueueEnabledMissing("RepeatQueue")
	}
	if !*q.Enabled {
		return config.RepeatQueueConfig{}, nil
	}
	if q.WorkerCount <= 0 {
		return config.RepeatQueueConfig{}, fmt.Errorf("RepeatQueue.WorkerCount 必须 > 0" +
			"（为 0 时池子里没有 worker，到点的任务会静静躺在队列里）")
	}
	if q.BatchSize <= 0 {
		return config.RepeatQueueConfig{}, fmt.Errorf("RepeatQueue.BatchSize 必须 > 0（每批处理多少条；0 = 一条都不处理）")
	}
	if q.Interval == "" {
		return config.RepeatQueueConfig{}, fmt.Errorf(`RepeatQueue.Interval 必填："2h" 这种最粗兜底的扫描间隔；` +
			`不自动跑用 SiteSpec.AutoRepeat 关`)
	}
	interval, err := time.ParseDuration(q.Interval)
	if err != nil {
		return config.RepeatQueueConfig{}, fmt.Errorf("RepeatQueue.Interval 无法解析: %w", err)
	}
	if interval <= 0 {
		return config.RepeatQueueConfig{}, fmt.Errorf(`RepeatQueue.Interval 必须 > 0；` +
			`"不自动跑"用 SiteSpec.AutoRepeat 关，别在这里写 "0"`)
	}
	return config.RepeatQueueConfig{
		Enabled:     true,
		WorkerCount: q.WorkerCount,
		Interval:    interval,
		BatchSize:   q.BatchSize,
	}, nil
}

// stagePlan 一条阶段声明的落地计划：校验通过后算出来的"要做的事"。
//
// 校验与策略判断都在这层做完（纯函数，不需要引擎 → 可单测），applyStagePlan 只负责落地+打日志。
// 这样"引擎只给什么跑什么、策略在声明层"这条分工是结构上保证的。
type stagePlan struct {
	stage   string
	cfg     engine.StageConfig
	fetcher engine.Fetcher
	sub     func(*engine.Engine)
	note    string // 启动时打的一句话（空 = 不打）
	warn    bool   // note 是否按 WARN 级别打
}

// planStage 校验一条阶段声明并算出落地计划；**纯函数**（不碰 Engine）。
func planStage(site core.Site, st StageSpec, owner map[string]string) (stagePlan, error) {
	var plan stagePlan
	if st.Fetcher == nil {
		return plan, fmt.Errorf("fetcher 为 nil")
	}
	stage := strings.TrimSpace(st.Fetcher.GetStage())
	if stage == "" {
		return plan, fmt.Errorf("fetcher 的 GetStage() 返回空串")
	}
	if prev, dup := owner[stage]; dup {
		return plan, fmt.Errorf("阶段 %q 重复声明（已在 %s 声明过）—— 多站时阶段名要能区分，"+
			"约定带站点前缀，如 %s_%s", stage, prev, site.Key, stage)
	}
	if st.WorkerCount <= 0 {
		return plan, fmt.Errorf("阶段 %q: WorkerCount 必须 > 0（为 0 时池子里没有 worker，任务会静静躺在队列里）", stage)
	}
	if st.QueueSize <= 0 {
		return plan, fmt.Errorf("阶段 %q: QueueSize 必须 > 0（为 0 时高水位判定恒真，每次提交都溢出到 DB）", stage)
	}
	delay, err := parseDelay(st.Delay)
	if err != nil {
		return plan, fmt.Errorf("阶段 %q: Delay %w（写法同 YAML：\"5m\" 或 \"10s-30s\"）", stage, err)
	}
	backoff, err := parseBackoff(st.Retry.Backoff)
	if err != nil {
		return plan, fmt.Errorf("阶段 %q: Retry.Backoff %w（写法同 YAML：\"30s\"）", stage, err)
	}
	attempts := st.Retry.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	owner[stage] = siteLabel(site, 0)

	sub := entryFunc(st.Fetcher, site)
	plan = stagePlan{
		stage:   stage,
		fetcher: st.Fetcher,
		cfg: engine.StageConfig{
			MaxAttempts: attempts,
			Backoff:     backoff,
			WorkerCount: st.WorkerCount,
			QueueSize:   st.QueueSize,
			Delay:       delay,
			Site:        site.Key,
		},
	}
	switch {
	case st.AutoStart && sub == nil:
		// 开了开关却没人实现入口：配置与实现对不上，值得说一句（不 panic —— 入口可能回头再补）
		plan.note, plan.warn = fmt.Sprintf("阶段 %s: AutoStart=true，但它的 fetcher 没有实现 SubmitEntries —— 没有入口任务可投", stage), true
	case !st.AutoStart && sub != nil:
		// 实现了入口却没开开关：说一句"是关的、不是漏的"（少写一个 AutoStart: true 是很容易犯的错）
		plan.note = fmt.Sprintf("阶段 %s: 实现了入口但 AutoStart=false，本次启动不投入口任务", stage)
		sub = nil
	}
	plan.sub = sub
	return plan, nil
}

// applyStagePlan 落地一条计划（注册阶段 + 打它带来的那句话）。
func (a *App) applyStagePlan(plan stagePlan) {
	a.Engine.AddStage(plan.stage, plan.cfg, plan.fetcher, plan.sub)
	if plan.note == "" {
		return
	}
	if plan.warn {
		a.Logger.Engine.Warn(plan.note)
		return
	}
	a.Logger.Engine.Info(plan.note)
}

// entryFunc 从 fetcher 上取入口回调：实现了 EntrySubmitter 就用它（把本站声明的快照一并交给它），
// 否则没有入口（nil）。于是"这个阶段有没有起始任务"只由 fetcher 自己说了算，声明处不用（也不该）再写一遍。
func entryFunc(f engine.Fetcher, site core.Site) func(*engine.Engine) {
	es, ok := f.(engine.EntrySubmitter)
	if !ok {
		return nil
	}
	return func(e *engine.Engine) { es.SubmitEntries(e, site) }
}

// Site 取某个站点的声明（BaseURL 等）。handler 里通常用 `engine.Site(task.Site)`。
func (a *App) Site(key string) (core.Site, bool) {
	s, ok := a.sites[key]
	return s, ok
}

func siteLabel(site core.Site, idx int) string {
	if site.Key != "" {
		return "站点 " + site.Key
	}
	return fmt.Sprintf("第 %d 个站点（未命名）", idx+1)
}

// parseDelay 解析阶段间隔；空串 = 不延迟（零值区间）。
func parseDelay(s string) (config.DurationRange, error) {
	if strings.TrimSpace(s) == "" {
		return config.DurationRange{}, nil
	}
	r, err := config.ParseDurationRange(s)
	if err != nil {
		return config.DurationRange{}, fmt.Errorf("无法解析: %w", err)
	}
	return r, nil
}

// parseBackoff 解析退避基数；空串 = 1s（与原来配置里的默认值一致）。
func parseBackoff(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("无法解析: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("必须 > 0（实得 %s）", d)
	}
	return d, nil
}

// toConfig 把站点自己的熔断声明合到默认值上；未给的字段用默认值。
func (b BreakerSpec) toConfig(def config.BreakerConfig) (breaker.Config, error) {
	out := breaker.Config{
		Enabled:   b.Enabled, // 不看默认：写了就由它说了算（见 BreakerSpec.Enabled）
		Threshold: def.Threshold,
		Window:    def.WindowOrDefault(),
	}
	if b.Threshold != 0 {
		out.Threshold = b.Threshold
	}
	if strings.TrimSpace(b.Window) != "" {
		w, err := time.ParseDuration(b.Window)
		if err != nil {
			return out, fmt.Errorf("Window 无法解析: %w", err)
		}
		out.Window = w
	}
	return out, nil
}
