package obs

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/obs/eventlog"
	"github.com/ydtg1993/papa/v4/obs/logidx"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// logSlack 是日志证据窗口的前后冗余。
//
// 事件 ts 是毫秒精度，而人类日志时间戳是**秒**精度；严格用 [from,to] 会把与窗口起点同一秒
// 的日志（如启动那一批）全部切掉（实测：narrow window 下 0 签名、放宽后 40 个）。
const logSlack = 5 * time.Minute

// SourceConfig 是加载报告输入（事件流 + 日志证据）所需的路径与窗口。
type SourceConfig struct {
	// EventsFile 事件流主文件（通常是 log.dir/events.jsonl）。
	EventsFile string
	// LogDir 人类日志目录；IncludeLogs=false 时忽略。
	LogDir string
	// Since/Until 时间窗口；零值表示按事件里的最早/最晚时刻推断。
	Since, Until time.Time
	// RunID 非空时只保留该 run 的事件。
	RunID string
	// IncludeLogs 是否做日志证据索引（A 档：只做证据，不产生指标）。
	IncludeLogs bool
	// Loc 解析日志里无时区时间戳的时区；nil = time.Local。
	Loc *time.Location
}

// Sources 是加载好的一次运行输入。
type Sources struct {
	Run       RunMeta
	EventMeta EventMeta
	Events    []events.Event
	Logs      LogSummary
	Degraded  []string
}

// LoadSources 读取事件流（含轮转备份/.gz）、按窗口过滤，并可选地对日志做证据索引。
//
// 它是 `papa report` 与业务侧报告命令共用的加载器：报告引擎本身不碰文件系统。
func LoadSources(sc SourceConfig) (*Sources, error) {
	return LoadSourcesContext(context.Background(), sc, inputbudget.Limits{})
}
func LoadSourcesContext(ctx context.Context, sc SourceConfig, limits inputbudget.Limits) (*Sources, error) {
	if sc.EventsFile == "" {
		return nil, fmt.Errorf("事件文件路径为空")
	}
	paths, err := eventlog.DiscoverContext(ctx, sc.EventsFile, limits)
	if err != nil {
		return nil, fmt.Errorf("扫描事件文件: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("没有找到事件文件 %s —— 确认 obs.events.enabled 已打开并跑过一轮", sc.EventsFile)
	}
	res, err := eventlog.ReadContext(ctx, paths, sc.Since, sc.Until, sc.RunID, limits)
	if err != nil {
		return nil, fmt.Errorf("读取事件: %w", err)
	}
	evs := res.Events
	gaps := res.Gaps
	if sc.RunID != "" {
		evs = FilterRunID(evs, sc.RunID)
		if len(evs) == 0 {
			return nil, fmt.Errorf("事件里没有 run_id=%s 的记录", sc.RunID)
		}
		gaps = eventlog.CountGaps(evs)
	}
	from, to := sc.Since, sc.Until
	if from.IsZero() || to.IsZero() {
		f, t := EventWindow(evs)
		if from.IsZero() {
			from = f
		}
		if to.IsZero() {
			to = t
		}
	}

	out := &Sources{
		Run:       RunMeta{RunIDs: CollectRunIDs(evs), From: from, To: to},
		EventMeta: EventMeta{Count: len(evs), Files: res.Files, Gaps: gaps, Unparsed: res.Unparsed, Duplicates: res.Duplicates},
		Events:    evs,
	}
	if sc.IncludeLogs {
		logFrom, logTo := from, to
		if !logFrom.IsZero() {
			logFrom = logFrom.Add(-logSlack)
		}
		if !logTo.IsZero() {
			logTo = logTo.Add(logSlack)
		}
		summary, err := LoadLogSummaryContext(ctx, sc.LogDir, logFrom, logTo, sc.Loc, limits)
		if err != nil {
			if errors.Is(err, inputbudget.ErrLimit) || ctx.Err() != nil {
				return nil, err
			}
			out.Degraded = append(out.Degraded, "日志证据索引失败: "+err.Error())
		} else {
			out.Logs = summary
		}
	}
	return out, nil
}

// LoadLogSummary 对目录下的人类日志做证据索引（*.log 与 *.log.gz）。
func LoadLogSummary(dir string, from, to time.Time, loc *time.Location) (LogSummary, error) {
	return LoadLogSummaryContext(context.Background(), dir, from, to, loc, inputbudget.Limits{})
}
func LoadLogSummaryContext(ctx context.Context, dir string, from, to time.Time, loc *time.Location, limits inputbudget.Limits) (LogSummary, error) {
	paths, err := discoverLogs(ctx, dir, limits)
	if err != nil {
		return LogSummary{}, err
	}
	if loc == nil {
		loc = time.Local
	}
	idx, err := logidx.BuildContext(ctx, logidx.Config{Paths: paths, From: from, To: to, Loc: loc}, limits)
	if err != nil {
		return LogSummary{}, err
	}
	return logSummary(idx), nil
}
func logSummary(idx *logidx.Index) LogSummary {
	out := LogSummary{Files: idx.Files, Unparsed: idx.Unparsed, Truncated: idx.Truncated}
	for _, sig := range idx.Signatures {
		ls := LogSignature{Pattern: sig.Pattern, Count: sig.Count, First: sig.First, Last: sig.Last}
		for _, sample := range sig.Samples {
			ls.Samples = append(ls.Samples, LogSample{
				File: sample.File, Line: sample.Line, Level: sample.Level, Logger: sample.Logger, Msg: sample.Msg,
			})
		}
		out.Signatures = append(out.Signatures, ls)
	}
	return out
}

// FilterRunID 只保留指定 run_id 的事件。
func FilterRunID(evs []events.Event, runID string) []events.Event {
	out := make([]events.Event, 0)
	for _, ev := range evs {
		if ev.RunID == runID {
			out = append(out, ev)
		}
	}
	return out
}

// CollectRunIDs 按事件出现顺序去重收集 run_id。
func CollectRunIDs(evs []events.Event) []string {
	seen := make(map[string]bool, len(evs))
	out := make([]string, 0, 4)
	for _, ev := range evs {
		if ev.RunID == "" || seen[ev.RunID] {
			continue
		}
		seen[ev.RunID] = true
		out = append(out, ev.RunID)
	}
	return out
}

// EventWindow 返回事件里最早/最晚的时刻（跳过零值 TS）。
func EventWindow(evs []events.Event) (time.Time, time.Time) {
	var from, to time.Time
	for _, ev := range evs {
		if ev.TS.IsZero() {
			continue
		}
		if from.IsZero() || ev.TS.Before(from) {
			from = ev.TS
		}
		if to.IsZero() || ev.TS.After(to) {
			to = ev.TS
		}
	}
	return from, to
}

// discoverLogs bounds matching files without materializing an entire directory.
func discoverLogs(ctx context.Context, dir string, limits inputbudget.Limits) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("日志目录为空")
	}
	limits = limits.Normalize()
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var plain, gz []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, e := f.ReadDir(128)
		if e != nil && e != io.EOF {
			return nil, e
		}
		for _, de := range entries {
			if de.IsDir() {
				continue
			}
			n := de.Name()
			if strings.HasSuffix(n, ".log") {
				plain = append(plain, filepath.Join(dir, n))
			} else if strings.HasSuffix(n, ".log.gz") {
				gz = append(gz, filepath.Join(dir, n))
			} else {
				continue
			}
			if err := inputbudget.Check("files", int64(len(plain)+len(gz)), int64(limits.MaxFiles)); err != nil {
				return nil, err
			}
		}
		if e == io.EOF {
			break
		}
	}
	sort.Strings(plain)
	sort.Strings(gz)
	if len(plain)+len(gz) == 0 {
		return nil, fmt.Errorf("日志目录里没有 *.log")
	}
	return append(plain, gz...), nil
}
