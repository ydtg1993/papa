package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/obs/analyze"
	"github.com/ydtg1993/papa/v3/obs/pattern"
	"github.com/ydtg1993/papa/v3/obs/report"
)

const reportUsage = `usage:
  papa report generate [-c configs/config.yaml] [--since RFC3339] [--until RFC3339] [--run <run_id>] [--snapshot] [--out <dir>] [--no-logs] [--fail-on-verdict]
  papa report list [--out <dir>]

生成一次运行报告（只读）：读事件流 + 日志证据 + 框架状态，输出 report.md / report.json / ai-brief.md / ai-brief.json。

  -c/--config       配置文件路径（默认 PAPA_CONFIG，再回退 configs/config.yaml）
  --since/--until   时间窗口（RFC3339）；缺省按事件里的最早/最晚时刻
  --run             只保留指定 run_id，输出单份报告（目录名 = run_id；兼容旧的"单次运行"排查用法）
  --snapshot        强制快照模式：只出单日窗口的一份报告（跨天报错；验收请用默认 acceptance）
  --out             输出根目录（默认 obs.report.dir，再回退 ./reports）
  --no-logs         跳过日志证据索引（事件缺失时反而更快）
  --fail-on-verdict 任一日报判定为 fail 或 unknown 时以非零码退出（供 CI/自动化门禁）

产物默认**按本地日历日拆分**：有事件的每天各写一份 <dir>/<2006-01-02>/（同一天重跑会刷新当天那份）。
生成后按 obs.report.retention 清掉过期报告目录（默认 7 天；负数 = 永久保留）。

判定聚合：任一 fail 级检查未通过 → 不通过；只有 warn 未通过 → 带风险通过；否则通过。
业务报告段由业务进程注册（papa report 只有框架通用段）。`

// runReportCmd 解析 papa report 子命令。
func runReportCmd(args []string) error {
	if len(args) == 0 || args[0] != "generate" {
		if len(args) > 0 && args[0] == "list" {
			return runReportList(args[1:])
		}
		return errors.New(reportUsage)
	}
	opts, err := parseReportArgs(args[1:])
	if err != nil {
		return err
	}
	cfg, ok, err := loadOptionalConfig(opts.configPath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("读不到配置（用 -c 指定 configs/config.yaml，或设 PAPA_CONFIG）")
	}
	// 判定模式：配置 auto 时 CLI = acceptance（验收）；--snapshot 强制快照语义。
	mode, err := obs.ResolveMode(cfg.Obs.Report.Mode, obs.TriggerCLI)
	if err != nil {
		return err
	}
	if opts.snapshot {
		mode = obs.ModeSnapshot
	}

	evFile := cfg.Obs.Events.File
	if evFile == "" {
		evFile = "events.jsonl"
	}
	base := obs.SourceConfig{
		EventsFile:  filepath.Join(cfg.Log.Dir, evFile),
		LogDir:      cfg.Log.Dir,
		Since:       opts.since,
		Until:       opts.until,
		RunID:       opts.runID,
		IncludeLogs: !opts.noLogs,
		Loc:         time.Local,
	}
	// DB 连接失败只降级（报告跳过框架状态检查），不中断 —— 报告本身不能比被观测系统先死。
	db, dbErr := database.NewDB(cfg)
	if dbErr != nil {
		db = nil
	}
	// 框架内置段与 App 的退出/定时报告保持一致：通用概览 + 引擎状态 + 积压/老化。
	sections := []obs.Section{
		&analyze.Generic{Thresholds: analyze.Thresholds{
			StuckAfter:       cfg.Obs.Report.Thresholds.StuckAfter,
			CrawlFailRateMax: cfg.Obs.Report.Thresholds.CrawlFailRateMax,
			EventMaxGap:      cfg.Obs.Report.Thresholds.EventMaxGap,
		}},
		&analyze.EngineSection{StuckAfter: cfg.Obs.Report.Thresholds.StuckAfter},
		&analyze.BacklogSection{},
	}
	environment := map[string]string{
		"papa_version": runtimeVersion(),
		"events_file":  filepath.Join(cfg.Log.Dir, evFile),
		"log_dir":      cfg.Log.Dir,
	}
	// 引擎上下文：先给零值语义的 Source=none（不据此做暂停跳过）；首次拿到事件时
	// 再用 DB 快照 + 事件刷新一次（之后不再重复查库）。
	engine := obs.EngineState{Source: "none"}
	generate := func(src *obs.Sources) (*obs.Report, error) {
		if engine.Source == "none" && len(src.Events) > 0 {
			engine = analyze.LoadEngineState(context.Background(), db, src.Events, time.Now())
		}
		degraded := append([]string(nil), src.Degraded...)
		if dbErr != nil {
			degraded = append(degraded, "数据库连接失败（跳过框架状态检查）: "+dbErr.Error())
		}
		return obs.Generate(context.Background(), obs.Options{
			Run:         src.Run,
			EventMeta:   src.EventMeta,
			Events:      src.Events,
			Logs:        src.Logs,
			DB:          db,
			Environment: environment,
			Degraded:    degraded,
			Sections:    sections,
			Patterns:    pattern.Builtin(),
			Mode:        mode,
			Engine:      engine,
			Inputs:      reportInputs(cfg.Obs.Report.Thresholds, opts.configPath),
		})
	}

	outDir := opts.outDir
	if outDir == "" {
		outDir = cfg.Obs.Report.Dir
	}
	if outDir == "" {
		outDir = "./reports"
	}
	worst := obs.VerdictPass
	writeOne := func(label string, rep *obs.Report) error {
		files, err := report.Write(filepath.Join(outDir, label), rep)
		if err != nil {
			return err
		}
		switch rep.Verdict {
		case obs.VerdictFail:
			worst = obs.VerdictFail
		case obs.VerdictUnknown:
			// unknown 不是 pass：门禁下按不通过处理；但不该盖掉真正的 fail。
			if worst != obs.VerdictFail {
				worst = obs.VerdictUnknown
			}
		case obs.VerdictWarn:
			if worst != obs.VerdictFail && worst != obs.VerdictUnknown {
				worst = obs.VerdictWarn
			}
		}
		fmt.Printf("[%s] 判定：%s（%s）\n", label, report.VerdictLabel(rep.Verdict), rep.Verdict)
		fmt.Printf("       事件：%d 条 · 缺口 %d · 坏行 %d · 日志签名 %d\n",
			rep.EventMeta.Count, rep.EventMeta.Gaps, rep.EventMeta.Unparsed, len(rep.Logs.Signatures))
		for _, f := range files {
			fmt.Println("写出:", f)
		}
		return nil
	}

	if mode == obs.ModeSnapshot && opts.runID == "" {
		// --snapshot（且没有 --run）：只出「最后一个事件所在本地日」的一份快照报告。
		// 不能把全事件窗口直接交给 snapshotWindow —— 多天事件文件会误报跨天。
		probeCfg := base
		probeCfg.IncludeLogs = false // 只是探窗口，不必做两遍日志索引
		probe, err := obs.LoadSources(probeCfg)
		if err != nil {
			return err
		}
		if len(probe.Events) == 0 || probe.Run.To.IsZero() {
			return fmt.Errorf("snapshot 模式：窗口内没有事件，无法确定单日窗口（检查 obs.events 是否开启、或 --since/--until 是否填对）")
		}
		var day, end time.Time
		if opts.since.IsZero() && opts.until.IsZero() {
			// 缺省：最后一个事件所在**本地日**的 [当天 0 点, 最后事件时刻]。
			last := probe.Run.To.In(time.Local)
			day = time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, time.Local)
			end = probe.Run.To
		} else {
			// 显式窗口：收紧到本地日；跨日直接指路 acceptance（多日报告）。
			day, end, err = snapshotWindow(probe.Run.From, probe.Run.To, time.Local)
			if err != nil {
				return err
			}
		}
		base.Since, base.Until = day, end
		src, err := obs.LoadSources(base)
		if err != nil {
			return err
		}
		rep, err := generate(src)
		if err != nil {
			return fmt.Errorf("生成报告: %w", err)
		}
		if err := writeOne(report.DayName(day), rep); err != nil {
			return err
		}
	} else if opts.runID != "" {
		// 显式 --run：出一份单 run 报告（目录名 = run_id），方便排查某一次运行；不参与按天拆分。
		src, err := obs.LoadSources(base)
		if err != nil {
			return err
		}
		rep, err := generate(src)
		if err != nil {
			return fmt.Errorf("生成报告: %w", err)
		}
		if err := writeOne(report.DirName(rep.Run), rep); err != nil {
			return err
		}
	} else {
		// 默认按本地日历日拆分：有事件的每天各写一份 <dir>/<2006-01-02>/，重跑刷新当天那份。
		sources, err := obs.LoadDailySources(base)
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			return fmt.Errorf("窗口内没有事件（检查 obs.events 是否开启、或 --since/--until 是否填对）")
		}
		for _, src := range sources {
			day := report.DayName(src.Run.From)
			rep, err := generate(src)
			if err != nil {
				return fmt.Errorf("生成报告（%s）: %w", day, err)
			}
			if err := writeOne(day, rep); err != nil {
				return err
			}
		}
	}

	// 生成完顺手清过期报告（默认保留 7 天；obs.report.retention 负数 = 永久保留）。
	if removed, err := report.Prune(outDir, cfg.Obs.Report.Retention, time.Now(), time.Local); err != nil {
		fmt.Fprintf(os.Stderr, "清理过期报告失败：%v\n", err)
	} else if len(removed) > 0 {
		fmt.Printf("清理过期报告 %d 份：%s\n", len(removed), strings.Join(removed, ", "))
	}
	return verdictGate(worst, opts.failOnVerdict)
}

type reportArgs struct {
	configPath    string
	since         time.Time
	until         time.Time
	runID         string
	outDir        string
	noLogs        bool
	snapshot      bool
	failOnVerdict bool
}

func parseReportArgs(args []string) (reportArgs, error) {
	var out reportArgs
	take := func(i *int) (string, bool) {
		if *i+1 < len(args) {
			*i++
			return args[*i], true
		}
		return "", false
	}
	setTime := func(dst *time.Time, raw string) error {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return fmt.Errorf("时间格式应为 RFC3339（如 2026-10-28T09:00:00+08:00），实得 %q", raw)
		}
		*dst = t
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			v, ok := take(&i)
			if !ok {
				return out, errors.New("-c 缺少值")
			}
			out.configPath = v
		case strings.HasPrefix(a, "--config="):
			out.configPath = strings.TrimPrefix(a, "--config=")
		case a == "--since":
			v, ok := take(&i)
			if !ok {
				return out, errors.New("--since 缺少值")
			}
			if err := setTime(&out.since, v); err != nil {
				return out, err
			}
		case strings.HasPrefix(a, "--since="):
			if err := setTime(&out.since, strings.TrimPrefix(a, "--since=")); err != nil {
				return out, err
			}
		case a == "--until":
			v, ok := take(&i)
			if !ok {
				return out, errors.New("--until 缺少值")
			}
			if err := setTime(&out.until, v); err != nil {
				return out, err
			}
		case strings.HasPrefix(a, "--until="):
			if err := setTime(&out.until, strings.TrimPrefix(a, "--until=")); err != nil {
				return out, err
			}
		case a == "--run":
			v, ok := take(&i)
			if !ok {
				return out, errors.New("--run 缺少值")
			}
			out.runID = v
		case strings.HasPrefix(a, "--run="):
			out.runID = strings.TrimPrefix(a, "--run=")
		case a == "--snapshot":
			out.snapshot = true
		case a == "--out":
			v, ok := take(&i)
			if !ok {
				return out, errors.New("--out 缺少值")
			}
			out.outDir = v
		case strings.HasPrefix(a, "--out="):
			out.outDir = strings.TrimPrefix(a, "--out=")
		case a == "--no-logs":
			out.noLogs = true
		case a == "--fail-on-verdict":
			out.failOnVerdict = true
		default:
			return out, fmt.Errorf("未知参数 %q\n\n%s", a, reportUsage)
		}
	}
	return out, nil
}

// snapshotWindow 把窗口的起点收紧到**本地日**：返回当天 0 点与该窗口终点；跨日返回错误。
//
// snapshot 模式回答「现在这一刻的当天」，一份报告只覆盖一天 —— 多天窗口是 acceptance 语义。
func snapshotWindow(from, to time.Time, loc *time.Location) (time.Time, time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	// 按 loc 取日历日（from 可能带别的时区偏移，比如 UTC 的 --since）。
	f := from.In(loc)
	day := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, loc)
	t := to.In(loc)
	endDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	if !endDay.Equal(day) {
		return time.Time{}, time.Time{}, fmt.Errorf("snapshot 模式只支持单日窗口（%s ~ %s 跨天）；多日报告请用 acceptance 模式",
			from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"))
	}
	return day, to, nil
}

// verdictGate 是 --fail-on-verdict 的收口：fail 与 unknown 都算不通过 —— 门禁不能把
// 「没判定出来」当通过（数据缺失/暂停快照时 unknown 恰恰是最该人看的结果）。
func verdictGate(worst obs.Verdict, failOnVerdict bool) error {
	if failOnVerdict && (worst == obs.VerdictFail || worst == obs.VerdictUnknown) {
		return errors.New("判定为不通过或未判定（--fail-on-verdict）")
	}
	return nil
}

// runReportList 列出报告目录下的既有报告（按目录名排序）。
func runReportList(args []string) error {
	cfgPath, outDir := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config":
			if i+1 < len(args) {
				i++
				cfgPath = args[i]
			}
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case a == "--out":
			if i+1 < len(args) {
				i++
				outDir = args[i]
			}
		case strings.HasPrefix(a, "--out="):
			outDir = strings.TrimPrefix(a, "--out=")
		default:
			return fmt.Errorf("未知参数 %q", a)
		}
	}
	if outDir == "" {
		cfg, ok, err := loadOptionalConfig(cfgPath)
		if err != nil {
			return err
		}
		if ok {
			outDir = cfg.Obs.Report.Dir
		}
	}
	if outDir == "" {
		outDir = "./reports"
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return fmt.Errorf("读报告目录 %s: %w", outDir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			fmt.Println(filepath.Join(outDir, e.Name()))
		}
	}
	return nil
}

// runtimeVersion 返回当前 CLI 主模块版本（开发构建为 "(devel)"）。
func runtimeVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

// reportInputs 汇总 CLI 报告的自证信息：版本、配置路径与生效阈值。
//
// CLI 的 cfg 已经过 config.Load 归一，但这里仍按 analyze 的默认口径兜底 ——
// 将来若有人直接构造 Config，报告也不该把 0 当成生效阈值写出去。
// 时长统一换算成秒（FormatFloat 最短表示），与 App 的 obsReportInputs 保持同一口径。
func reportInputs(th config.ObsThresholdsConfig, configPath string) obs.Inputs {
	stuckAfter := 10 * time.Minute
	failRateMax := 0.05
	eventMaxGap := 10 * time.Minute
	if th.StuckAfter > 0 {
		stuckAfter = th.StuckAfter
	}
	if th.CrawlFailRateMax > 0 {
		failRateMax = th.CrawlFailRateMax
	}
	if th.EventMaxGap > 0 {
		eventMaxGap = th.EventMaxGap
	}
	return obs.Inputs{
		PapaVersion: runtimeVersion(),
		ConfigFile:  configPath,
		Thresholds: map[string]string{
			"stuck_after":         strconv.FormatFloat(stuckAfter.Seconds(), 'f', -1, 64),
			"crawl_fail_rate_max": strconv.FormatFloat(failRateMax, 'f', -1, 64),
			"event_max_gap":       strconv.FormatFloat(eventMaxGap.Seconds(), 'f', -1, 64),
		},
		ThresholdsSource: "effective（config 覆盖后，未校准）",
	}
}
