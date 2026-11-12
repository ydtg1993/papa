package sitedecl

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/breaker"
)

// Plan 一份站点声明**推导出来的落地内容**（纯数据）：校验与策略都在推导里做完，
// 应用层落地那一步只照做。RegisterSites 与 CheckSites 共用它 —— 两边的判据不会漂。
type Plan struct {
	Site      core.Site            // 声明快照（Key / BaseURL / Headers / …）
	Breaker   *breaker.Config      // 本站的熔断阈值；nil = 用默认那把
	Downloads engine.SiteDownloads // 本站的下载器覆盖层（nil 字段 = 不覆盖）
	Stages    []StagePlan
}

// StagePlan 一条阶段声明的落地计划：校验通过后算出来的"要做的事"。
//
// 校验与策略判断都在推导层做完（纯函数，不需要引擎 → 可单测），应用层只负责落地+打日志。
// 这样"引擎只给什么跑什么、策略在声明层"这条分工是结构上保证的。
type StagePlan struct {
	Stage   string
	Config  engine.StageConfig
	Fetcher engine.Fetcher
	Submit  func(*engine.Engine)
	Note    string // 启动时打的一句话（空 = 不打）
	Warn    bool   // Note 是否按 WARN 级别打
}

// CheckSites 校验一批站点与阶段声明，**不连库、不建池、不改任何状态**：
// 把 RegisterSites 会在启动时 panic 的那些问题**一次收集全**返回（nil = 都没问题）。
//
// 用途是"声明写完了先自查一遍"：真启动要连库、要建池，而这一步是纯推导 —— 阶段名重复、
// 三个治理队列漏写 / 缺项、熔断阈值、Delay 与 Backoff 的写法，都能在这儿查出来。
// cfg 提供全局默认（现在是 crawler.breaker 的默认阈值）；传 nil = 没有全局默认，只用声明里写的值。
func CheckSites(cfg *config.Config, sites ...SiteSpec) error {
	_, err := planSites(cfg, sites)
	return err
}

// PlanSites 校验并推导一批站点/阶段声明为纯数据计划（不碰引擎、不碰库）。
// 校验与推导在 planSites 里一次做完，有问题**一个字节都不落地**。
func PlanSites(cfg *config.Config, sites []SiteSpec) ([]Plan, error) {
	return planSites(cfg, sites)
}

// planSites 校验并推导一批声明；**把所有问题收集全**再一起返回（校验器要一次把话说全，
// 应用层拿到就 panic —— 等于"启动期一次性报全"，不用改一处跑一次）。
func planSites(cfg *config.Config, sites []SiteSpec) ([]Plan, error) {
	var errs []error
	if err := validateSites(sites); err != nil {
		errs = append(errs, err)
	}
	defBreaker := config.BreakerConfig{}
	if cfg != nil {
		defBreaker = cfg.Crawler.Breaker
	}
	// 阶段名只在**本站内**唯一（两个站可以各有自己的 catalog）—— 所以查重的键是
	// `site:stage` 的复合键，不是裸的阶段名。未命名的声明在上面就被 validateSites 拒掉了，
	// 所以这里的站点键一定非空；用 core.StageKey 拼只是为了"键只有一处拼法"。
	owner := make(map[string]string, len(sites)) // core.StageKey(site, stage) → 已声明的站点，用来查重
	plans := make([]Plan, 0, len(sites))
	for i, spec := range sites {
		plan, err := planSite(spec, i, defBreaker, owner)
		if err != nil {
			errs = append(errs, err)
		}
		plans = append(plans, plan)
	}
	return plans, errors.Join(errs...)
}

// planSite 一份声明 → 落地内容。纯函数：不碰引擎、不碰库，默认值由参数给（index 只用于报错文案）。
func planSite(spec SiteSpec, idx int, defBreaker config.BreakerConfig, owner map[string]string) (Plan, error) {
	var p Plan
	var errs []error
	p.Site = spec.snapshot() // 声明 → 框架内部流通的快照，一处转换
	// 下载器覆盖层原样交给引擎（解析时机在 ApplyRegisterStage：那时全局那份已经 Set 好了）
	p.Downloads = engine.SiteDownloads{M3U8: spec.M3U8, FileDown: spec.FileDown}

	// 熔断**每站一份计数**：声明里写了阈值就用它，没写就用 `crawler.breaker` 那份默认 ——
	// 要紧的是"每站各自一把"。共用的后果是 A 站被墙会把 B 站一起闸住（两个站同时被同一面墙
	// 挡住时看着"对"，但它们本该各自判定、各自恢复；一方先恢复时更明显）。
	// Key 一定非空（validateSites 已经拒掉没有 Key 的声明）—— 这里的判断纯属防御。
	if p.Site.Key != "" {
		bc := defaultBreakerConfig(defBreaker)
		usable := true
		if spec.Breaker != nil {
			declared, err := spec.Breaker.toConfig(defBreaker)
			if err == nil {
				// 阈值那条规则与 breaker.New 共用（enabled=true 时必须 > 0）—— 原来只有真起引擎时才炸，
				// 现在 CheckSites 也查得到。
				err = breaker.Validate(declared)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("站点 %q 的熔断配置: %w", p.Site.Key, err))
				usable = false
			} else {
				bc = declared
			}
		}
		if usable {
			p.Breaker = &bc
		}
	}

	// 三个治理队列现在是全局一段（config.yaml 的 error_queue / recover_queue / repeat_queue），
	// 站点声明不再带队列参数 —— 这里没有队列要推导。

	for j, st := range spec.Stages {
		sp, err := planStage(p.Site, st, owner)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s 的第 %d 个阶段: %w", siteLabel(p.Site, idx), j+1, err))
			continue
		}
		p.Stages = append(p.Stages, sp)
	}
	return p, errors.Join(errs...)
}

// planStage 校验一条阶段声明并算出落地计划；**纯函数**（不碰 Engine）。
func planStage(site core.Site, st StageSpec, owner map[string]string) (StagePlan, error) {
	var plan StagePlan
	if st.Fetcher == nil {
		return plan, fmt.Errorf("fetcher 为 nil")
	}
	stage := strings.TrimSpace(st.Fetcher.GetStage())
	if stage == "" {
		return plan, fmt.Errorf("fetcher 的 GetStage() 返回空串")
	}
	// 与站点键同一条道理：阶段键是 `site:stage` 拼的，名字里带 `:` 会和别的站点/阶段拼成同一个键。
	if strings.Contains(stage, ":") {
		return plan, fmt.Errorf("阶段名 %q 不能包含 `:` —— 阶段键按 `site:stage` 拼，"+
			"名字里带 `:` 会和别的站点/阶段拼出同一个键", stage)
	}
	key := core.StageKey(site.Key, stage)
	if _, dup := owner[key]; dup {
		return plan, fmt.Errorf("阶段 %q 在本站重复声明（%s 已经声明过）—— 同一站内阶段名必须唯一；"+
			"两个站之间同名是允许的（各自一份池子与统计）。若你本意是同一个站，"+
			"多半是复制站点文件后两份声明指向了同一个 fetcher", stage, siteLabel(site, 0))
	}
	if st.RepeatLimit < 0 {
		return plan, fmt.Errorf("阶段 %q: RepeatLimit 不能为负数", stage)
	}
	if st.BreakerReadOnlyCoverage != "" && (strings.TrimSpace(st.BreakerReadOnlyCoverage) == "" || len(st.BreakerReadOnlyCoverage) > 1024) {
		return plan, fmt.Errorf("阶段 %q: BreakerReadOnlyCoverage 必须描述完整只读覆盖范围，且不超过 1024 字节", stage)
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
	owner[key] = siteLabel(site, 0)

	sub := entryFunc(st.Fetcher, site)
	plan = StagePlan{
		Stage:   stage,
		Fetcher: st.Fetcher,
		Config: engine.StageConfig{
			BreakerReadOnlyCoverage: st.BreakerReadOnlyCoverage,
			RepeatLimit:             st.RepeatLimit,
			MaxAttempts:             attempts,
			Backoff:                 backoff,
			WorkerCount:             st.WorkerCount,
			QueueSize:               st.QueueSize,
			Delay:                   delay,
			Site:                    site.Key,
			Entry:                   st.Entry,
			M3U8:                    st.M3U8,
			FileDown:                st.FileDown,
		},
	}
	// 两态 × 有没有实现入口：四个格子各有各的说法，别让任一种组合静默过去。
	switch {
	case st.Entry && sub == nil:
		// 声明有入口却没人实现：配置与实现对不上（不 panic —— 入口可能回头再补）
		plan.Note, plan.Warn = fmt.Sprintf("站点 %s / 阶段 %s: 声明了 Entry（有入口任务），但它的 fetcher 没有实现 SubmitEntries —— 没有入口任务可投", site.Key, stage), true
	case !st.Entry && sub != nil:
		// false 是零值/默认值，所以"实现了入口却忘了声明"是**最常见**的那种手误 ——
		// 按 WARN 说，并且把话写全（比一句"是关的、不是漏的"有用）。
		plan.Note, plan.Warn = fmt.Sprintf("站点 %s / 阶段 %s: fetcher 实现了 SubmitEntries，但声明里没写 Entry —— "+
			"启动不投、这个入口形同不存在。想启动即投就写 Entry: true", site.Key, stage), true
		// 声明没写就等于"没有入口"：回调丢掉（与"fetcher 没实现 SubmitEntries"同一个状态）。
		sub = nil
	case st.Entry:
		// 阶段名只在本站内唯一：两个站都有 catalog 时，不写站点就是两行一模一样的日志。
		plan.Note = fmt.Sprintf("站点 %s / 阶段 %s: 有入口任务 —— 启动那一趟投一轮（SubmitEntries）", site.Key, stage)
	}
	plan.Submit = sub
	return plan, nil
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
