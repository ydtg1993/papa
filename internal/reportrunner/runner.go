// Package reportrunner owns scheduled/exit report generation and its drain lifecycle.
// It consumes snapshots and extensions, never an App or Engine.
package reportrunner

import (
	"context"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/analyze"
	"github.com/ydtg1993/papa/v4/obs/pattern"
	"github.com/ydtg1993/papa/v4/obs/report"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Logger interface {
	Infof(string, ...any)
	Warnf(string, ...any)
}

// Config is a value snapshot of only the settings owned by report generation.
// The host maps its configuration here before starting the runner.
type Config struct {
	Enabled                       bool
	LogDir, EventsFile, ReportDir string
	OnExit                        bool
	Interval, Timeout, Retention  time.Duration
	Mode                          string
	Thresholds                    analyze.Thresholds
}

type Options struct {
	Config      Config
	DB          *gorm.DB
	Logger      Logger
	Sections    []obs.Section
	Patterns    []obs.Pattern
	EngineState func() obs.EngineState
	Inputs      func() obs.Inputs
	Version     string
}
type Runner struct {
	options     Options
	generateMu  sync.Mutex
	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

func New(options Options) *Runner {
	options.Sections = append([]obs.Section(nil), options.Sections...)
	options.Patterns = append([]obs.Pattern(nil), options.Patterns...)
	if options.EngineState == nil {
		options.EngineState = func() obs.EngineState { return obs.EngineState{Source: "none", CapturedAt: time.Now()} }
	}
	if options.Inputs == nil {
		options.Inputs = func() obs.Inputs { return obs.Inputs{} }
	}
	return &Runner{options: options}
}

// Start is idempotent until Stop. Host configuration is fixed before startup.
func (r *Runner) Start() {
	cfg := r.options.Config
	if !cfg.Enabled || cfg.Interval <= 0 {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		r.options.Logger.Infof("auto report enabled: interval=%s dir=%s", cfg.Interval, cfg.ReportDir)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ctx.Err() != nil {
					return
				}
				r.Periodic()
			}
		}
	}()
}

// Stop waits for the current report before the host closes storage. Repeated calls are safe.
func (r *Runner) Stop() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel = nil
	r.done = nil
}
func (r *Runner) Exit() {
	if r.options.Config.OnExit {
		r.Generate(obs.TriggerExit)
	}
}
func (r *Runner) Periodic() {
	cfg := r.options.Config
	if cfg.Enabled && cfg.Interval > 0 {
		r.Generate(obs.TriggerPeriodic)
	}
}
func (r *Runner) Generate(trigger obs.Trigger) {
	cfg := r.options.Config
	if !cfg.Enabled {
		return
	}
	// 串行化：定时报告与退出报告都写「当天」目录，不能同时写同一批文件。
	r.generateMu.Lock()
	defer r.generateMu.Unlock()

	// 触发方式决定默认语义：定时与正常退出均为 snapshot；验收须显式配置 acceptance。
	// 配置可强制覆盖；未知值记录警告并回退运行快照。
	mode, err := obs.ResolveMode(cfg.Mode, trigger)
	if err != nil {
		r.options.Logger.Warnf("%s: 模式解析失败，回退 snapshot：%s", trigger, err.Error())
		mode = obs.ModeSnapshot
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	evFile := cfg.EventsFile
	if evFile == "" {
		evFile = "events.jsonl"
	}
	now := time.Now()
	base := obs.SourceConfig{
		EventsFile:  filepath.Join(cfg.LogDir, evFile),
		LogDir:      cfg.LogDir,
		IncludeLogs: true,
		Loc:         time.Local,
	}
	if mode == obs.ModeSnapshot {
		base.Since = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		base.Until = now
		// 窗口 = [当天 0 点, 生成时刻]：snapshot 报的是「现在这一刻的当天」，
		// 上界钉在 now —— 晚于生成时刻的事件（时钟偏差、补写、未来时间戳）不算进来。
		// 代价是测试 fixture 必须落在窗口内：凌晨跑时把事件写在中午会被裁掉，报告因「没有事件」跳过。
	}
	sources, err := obs.LoadDailySources(base)
	if err != nil {
		r.options.Logger.Warnf("%s: 读取事件失败，跳过：%s", trigger, err.Error())
		return
	}
	if len(sources) == 0 {
		r.options.Logger.Warnf("%s: 事件窗口里没有事件，跳过", trigger)
		return
	}

	// 框架内置段：通用概览 + 引擎状态 + 积压/老化。顺序即渲染顺序；
	// 业务段始终排在框架段之后（业务段看到的是同一份 Input，不改变框架口径）。
	sections := []obs.Section{
		&analyze.Generic{Thresholds: analyze.Thresholds{
			StuckAfter:       cfg.Thresholds.StuckAfter,
			CrawlFailRateMax: cfg.Thresholds.CrawlFailRateMax,
			EventMaxGap:      cfg.Thresholds.EventMaxGap,
		}},
		&analyze.EngineSection{StuckAfter: cfg.Thresholds.StuckAfter},
		&analyze.BacklogSection{},
	}
	sections = append(sections, r.options.Sections...)
	patterns := append(pattern.Builtin(), r.options.Patterns...)

	engine := r.options.EngineState()
	written := 0
	for _, src := range sources {
		if ctx.Err() != nil {
			r.options.Logger.Warnf("%s: 生成超时（%s），剩余日报跳过", trigger, timeout)
			break
		}
		// Recovering 是「这一天窗口内有没有启动恢复」的逐日报事实：按源填，不能把
		// 进程级 engine 快照原样复用，否则某一天的恢复会染到其它天的报告上。
		// 口径与 analyze.LoadEngineState 的离线实现保持一致。
		eng := engine
		eng.Recovering = recoverRequeueSeen(src.Events)
		rep, err := obs.Generate(ctx, obs.Options{
			Run:         src.Run,
			EventMeta:   src.EventMeta,
			Events:      src.Events,
			Logs:        src.Logs,
			DB:          r.options.DB,
			Environment: map[string]string{"papa_version": r.options.Version},
			Degraded:    src.Degraded,
			Sections:    sections,
			Patterns:    patterns,
			Mode:        mode,
			Engine:      eng,
			Inputs:      r.options.Inputs(),
		})
		if err != nil {
			r.options.Logger.Warnf("%s: 生成失败：%s", trigger, err.Error())
			continue
		}
		day := report.DayName(src.Run.From)
		dir := filepath.Join(cfg.ReportDir, day)
		if _, err := report.Write(dir, rep); err != nil {
			r.options.Logger.Warnf("%s: 写盘失败（%s）：%s", trigger, day, err.Error())
			continue
		}
		written++
		r.options.Logger.Infof("%s: %s verdict=%s dir=%s", trigger, day, rep.Verdict, dir)
	}
	if written == 0 {
		r.options.Logger.Warnf("%s: 没有写出任何日报", trigger)
	}
	if removed, err := report.Prune(cfg.ReportDir, cfg.Retention, time.Now(), time.Local); err != nil {
		r.options.Logger.Warnf("%s: 清理过期报告失败：%s", trigger, err.Error())
	} else if len(removed) > 0 {
		r.options.Logger.Infof("%s: 已清理过期报告 %d 份：%s", trigger, len(removed), strings.Join(removed, ", "))
	}
}

// recoverRequeueSeen 判断这一天的窗口里是否出现过启动恢复重投
// （task_requeued 且 queue 名含 recover）。与 analyze.LoadEngineState 的离线口径一致：
// 离线报告能从事件看出的「正在恢复」，进程内报告也必须看出，否则同一份数据两种结论。
func recoverRequeueSeen(evs []events.Event) bool {
	for _, ev := range evs {
		if ev.Type == events.TypeTaskRequeued && strings.Contains(strings.ToLower(ev.Queue), "recover") {
			return true
		}
	}
	return false
}
