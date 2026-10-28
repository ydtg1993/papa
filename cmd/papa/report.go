package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/obs/analyze"
	"github.com/ydtg1993/papa/v3/obs/pattern"
	"github.com/ydtg1993/papa/v3/obs/report"
)

const reportUsage = `usage:
  papa report generate [-c configs/config.yaml] [--since RFC3339] [--until RFC3339] [--run <run_id>] [--out <dir>] [--no-logs] [--fail-on-verdict]
  papa report list [--out <dir>]

生成一次运行报告（只读）：读事件流 + 日志证据 + 框架状态，输出 report.md / report.json。

  -c/--config       配置文件路径（默认 PAPA_CONFIG，再回退 configs/config.yaml）
  --since/--until   时间窗口（RFC3339）；缺省按事件里的最早/最晚时刻
  --run             只保留指定 run_id；缺省合并窗口内所有 run
  --out             输出根目录（默认 obs.report.dir，再回退 ./reports）
  --no-logs         跳过日志证据索引（事件缺失时反而更快）
  --fail-on-verdict 判定为"不通过"时以非零码退出（供 CI/自动化门禁）

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

	evFile := cfg.Obs.Events.File
	if evFile == "" {
		evFile = "events.jsonl"
	}
	src, err := obs.LoadSources(obs.SourceConfig{
		EventsFile:  filepath.Join(cfg.Log.Dir, evFile),
		LogDir:      cfg.Log.Dir,
		Since:       opts.since,
		Until:       opts.until,
		RunID:       opts.runID,
		IncludeLogs: !opts.noLogs,
		Loc:         time.Local,
	})
	if err != nil {
		return err
	}

	degraded := append([]string(nil), src.Degraded...)
	db, err := database.NewDB(cfg)
	if err != nil {
		degraded = append(degraded, "数据库连接失败（跳过框架状态检查）: "+err.Error())
		db = nil
	}

	rep, err := obs.Generate(context.Background(), obs.Options{
		Run:       src.Run,
		EventMeta: src.EventMeta,
		Events:    src.Events,
		Logs:      src.Logs,
		DB:        db,
		Environment: map[string]string{
			"papa_version": runtimeVersion(),
			"events_file":  filepath.Join(cfg.Log.Dir, evFile),
			"log_dir":      cfg.Log.Dir,
		},
		Degraded: degraded,
		Sections: []obs.Section{&analyze.Generic{Thresholds: analyze.Thresholds{
			StuckAfter:       cfg.Obs.Report.Thresholds.StuckAfter,
			CrawlFailRateMax: cfg.Obs.Report.Thresholds.CrawlFailRateMax,
		}}},
		Patterns: pattern.Builtin(),
	})
	if err != nil {
		return fmt.Errorf("生成报告: %w", err)
	}

	outDir := opts.outDir
	if outDir == "" {
		outDir = cfg.Obs.Report.Dir
	}
	if outDir == "" {
		outDir = "./reports"
	}
	dir := filepath.Join(outDir, report.DirName(rep.Run))
	files, err := report.Write(dir, rep)
	if err != nil {
		return err
	}
	fmt.Printf("判定：%s（%s）\n", report.VerdictLabel(rep.Verdict), rep.Verdict)
	fmt.Printf("事件：%d 条 · 缺口 %d · 坏行 %d · 日志签名 %d\n",
		rep.EventMeta.Count, rep.EventMeta.Gaps, rep.EventMeta.Unparsed, len(rep.Logs.Signatures))
	for _, f := range files {
		fmt.Println("写出:", f)
	}
	if opts.failOnVerdict && rep.Verdict == obs.VerdictFail {
		return errors.New("判定为不通过（--fail-on-verdict）")
	}
	return nil
}

type reportArgs struct {
	configPath    string
	since         time.Time
	until         time.Time
	runID         string
	outDir        string
	noLogs        bool
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
