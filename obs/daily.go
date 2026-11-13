package obs

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/obs/eventlog"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"github.com/ydtg1993/papa/v4/obs/logidx"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LoadDailySources collects bounded input; prefer ConsumeDailySources to release each day.
func LoadDailySources(sc SourceConfig) ([]*Sources, error) {
	out := make([]*Sources, 0)
	err := ConsumeDailySources(context.Background(), sc, inputbudget.Limits{}, func(src *Sources) error { out = append(out, src); return nil })
	if err != nil {
		return nil, err
	}
	return out, nil
}

type dailyKey struct {
	run string
	seq uint64
}
type dailyBucket struct {
	day        time.Time
	path       string
	selected   bool
	seen       map[dailyKey]struct{}
	duplicates int
	bytes      int64
}

// ConsumeDailySources scans original sources once and stages bounded daily
// buckets. All input limits are checked before the first callback. Temporary
// files are removed on every return; callbacks own any Sources they retain.
func ConsumeDailySources(ctx context.Context, sc SourceConfig, limits inputbudget.Limits, consume func(*Sources) error) error {
	limits = limits.Normalize()
	if sc.EventsFile == "" {
		return fmt.Errorf("事件文件路径为空")
	}
	paths, err := eventlog.DiscoverContext(ctx, sc.EventsFile, limits)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("没有找到事件文件 %s", sc.EventsFile)
	}
	dir, err := os.MkdirTemp("", "papa-report-input-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	loc := sc.Loc
	if loc == nil {
		loc = time.Local
	}
	buckets := make(map[string]*dailyBucket)
	global := make(map[dailyKey]struct{})
	var spool, keyBytes int64
	keys := 0
	matched := false
	meta, err := eventlog.ScanContext(ctx, paths, sc.Since, sc.Until, limits, func(ev events.Event, raw string) error {
		key := dailyKey{ev.RunID, ev.Seq}
		_, duplicate := global[key]
		if !duplicate {
			if err := inputbudget.Check("dedup keys", int64(len(global)+1), int64(limits.MaxDedupKeys)); err != nil {
				return err
			}
			keyBytes += int64(len(ev.RunID))
			if err := inputbudget.Check("dedup key bytes", keyBytes, limits.MaxRetainedBytes); err != nil {
				return err
			}
			global[key] = struct{}{}
			if sc.RunID == "" || ev.RunID == sc.RunID {
				matched = true
			}
		}
		if ev.TS.IsZero() {
			return nil
		}
		t := ev.TS.In(loc)
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		name := day.Format("2006-01-02")
		b := buckets[name]
		if b == nil {
			if err := inputbudget.Check("days", int64(len(buckets)+1), int64(limits.MaxDays)); err != nil {
				return err
			}
			b = &dailyBucket{day: day, path: filepath.Join(dir, fmt.Sprintf("%d.jsonl", len(buckets))), seen: make(map[dailyKey]struct{})}
			buckets[name] = b
		}
		if !duplicate && (sc.RunID == "" || ev.RunID == sc.RunID) {
			b.selected = true
		}
		if _, ok := b.seen[key]; ok {
			b.duplicates++
			return nil
		}
		keys++
		if err := inputbudget.Check("daily dedup keys", int64(keys), int64(limits.MaxDedupKeys)); err != nil {
			return err
		}
		keyBytes += int64(len(ev.RunID))
		if err := inputbudget.Check("dedup key bytes", keyBytes, limits.MaxRetainedBytes); err != nil {
			return err
		}
		b.seen[key] = struct{}{}
		if !strings.HasSuffix(raw, "\n") {
			raw += "\n"
		}
		b.bytes += int64(len(raw))
		spool += int64(len(raw))
		if err := inputbudget.Check("retained event bytes", b.bytes, limits.MaxRetainedBytes); err != nil {
			return err
		}
		if err := inputbudget.Check("spool bytes", spool, limits.MaxSpoolBytes); err != nil {
			return err
		}
		f, err := os.OpenFile(b.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(raw)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if sc.RunID != "" && !matched {
		return fmt.Errorf("事件里没有 run_id=%s 的记录", sc.RunID)
	}
	days := make([]*dailyBucket, 0, len(buckets))
	for _, b := range buckets {
		b.seen = nil
		if b.selected {
			days = append(days, b)
		}
	}
	global = nil
	sort.Slice(days, func(i, j int) bool { return days[i].day.Before(days[j].day) })
	windows := make([]logidx.Window, len(days))
	for i, b := range days {
		from, to := dailyWindow(sc, b.day)
		windows[i] = logidx.Window{From: from.Add(-logSlack), To: to.Add(logSlack)}
	}
	var logs []*logidx.Index
	var degraded []string
	if sc.IncludeLogs && len(days) > 0 {
		paths, e := discoverLogs(ctx, sc.LogDir, limits)
		if e == nil {
			logs, e = logidx.BuildWindowsContext(ctx, logidx.Config{Paths: paths, Loc: loc}, windows, limits)
		}
		if e != nil {
			if errors.Is(e, inputbudget.ErrLimit) || ctx.Err() != nil {
				return e
			}
			degraded = []string{"日志证据索引失败: " + e.Error()}
		}
	}
	for i, b := range days {
		if err := ctx.Err(); err != nil {
			return err
		}
		from, to := dailyWindow(sc, b.day)
		// A final original line may lack a newline; staging adds one delimiter.
		spoolLimits := limits
		if spoolLimits.MaxLineBytes < int64(^uint64(0)>>1) {
			spoolLimits.MaxLineBytes++
		}
		if spoolLimits.MaxBytes < spoolLimits.MaxRetainedBytes {
			spoolLimits.MaxBytes = spoolLimits.MaxRetainedBytes
		}
		res, err := eventlog.ReadContext(ctx, []string{b.path}, from, to, sc.RunID, spoolLimits)
		if err != nil {
			return err
		}
		src := &Sources{Run: RunMeta{RunIDs: CollectRunIDs(res.Events), From: from, To: to}, Events: res.Events, EventMeta: EventMeta{Count: len(res.Events), Files: meta.Files, Gaps: res.Gaps, Unparsed: meta.Unparsed, Duplicates: b.duplicates}, Degraded: append([]string(nil), degraded...)}
		if logs != nil {
			src.Logs = logSummary(logs[i])
			logs[i] = nil
		}
		if err := consume(src); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func dailyWindow(sc SourceConfig, day time.Time) (time.Time, time.Time) {
	from, to := day, day.AddDate(0, 0, 1).Add(-time.Nanosecond)
	if !sc.Since.IsZero() && sc.Since.After(from) {
		from = sc.Since
	}
	if !sc.Until.IsZero() && sc.Until.Before(to) {
		to = sc.Until
	}
	return from, to
}
