package eventlog

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/events"
)

// eventKey 是事件去重键：同一 run 内 seq 唯一。
type eventKey struct {
	runID string
	seq   uint64
}

// Read 读取 paths（以 .gz 结尾的按 gzip 解压），逐行解析 JSONL，只保留 TS 落在
// [from, to] 闭区间内的事件，再按 (run_id, seq) 去重、按 TS 升序排序。
//
// 窗口边界：from 为零值表示不限下界，to 为零值表示不限上界。
// 单行解析失败计入 Result.Unparsed 并继续读取；空行忽略；文件打不开或读失败
// 返回 error（此时 Result 为零值）。
//
// 去重在窗口过滤之后进行：同一 (run_id, seq) 保留输入顺序中窗口内的第一条，
// 因此重复传入同一路径不会产生重复事件，重复条数记入 Result.Duplicates。
func Read(paths []string, from, to time.Time) (Result, error) {
	return ReadContext(context.Background(), paths, from, to, "", inputbudget.Limits{})
}

// ReadContext preserves metadata over all runs but retains only runID when set.
func ReadContext(ctx context.Context, paths []string, from, to time.Time, runID string, limits inputbudget.Limits) (Result, error) {
	limits = limits.Normalize()
	res := Result{Files: len(paths)}
	seen := make(map[eventKey]struct{})
	var retained int64
	meta, err := ScanContext(ctx, paths, from, to, limits, func(ev events.Event, raw string) error {
		key := eventKey{ev.RunID, ev.Seq}
		if _, ok := seen[key]; ok {
			res.Duplicates++
			return nil
		}
		if err := inputbudget.Check("dedup keys", int64(len(seen)+1), int64(limits.MaxDedupKeys)); err != nil {
			return err
		}
		retained += int64(len(raw))
		if err := inputbudget.Check("retained event bytes", retained, limits.MaxRetainedBytes); err != nil {
			return err
		}
		seen[key] = struct{}{}
		if runID == "" || ev.RunID == runID {
			res.Events = append(res.Events, ev)
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	res.Unparsed = meta.Unparsed
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	sortEvents(res.Events)
	res.Gaps = CountGaps(res.Events)
	return res, ctx.Err()
}

// ScanContext decodes window-matching events in input order including duplicates.
// Unparsed counts malformed or unsupported-version nonempty lines. Raw includes its newline.
func ScanContext(ctx context.Context, paths []string, from, to time.Time, limits inputbudget.Limits, yield func(events.Event, string) error) (Result, error) {
	meter := inputbudget.New(limits)
	if err := inputbudget.Check("files", int64(len(paths)), int64(meter.Limits.MaxFiles)); err != nil {
		return Result{}, err
	}
	res := Result{Files: len(paths)}
	count := 0
	for _, path := range paths {
		err := scanFile(ctx, path, meter, func(raw string) error {
			line := strings.TrimSpace(raw)
			if line == "" {
				return nil
			}
			var ev events.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.V != events.Version {
				res.Unparsed++
				return nil
			}
			if !inWindow(ev.TS, from, to) {
				return nil
			}
			count++
			if err := inputbudget.Check("events", int64(count), int64(meter.Limits.MaxEvents)); err != nil {
				return err
			}
			return yield(ev, raw)
		})
		if err != nil {
			return Result{}, fmt.Errorf("eventlog: read %q: %w", path, err)
		}
	}
	return res, ctx.Err()
}
func scanFile(ctx context.Context, path string, meter *inputbudget.Meter, yield func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = inputbudget.Reader(ctx, f)
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	br := bufio.NewReader(r)
	for {
		line, err := meter.Line(ctx, br)
		if err != nil && err != io.EOF {
			return err
		}
		if line != "" {
			if e := yield(line); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return ctx.Err()
		}
	}
}

// inWindow 判断 ts 是否落在 [from, to] 闭区间内；零值边界表示该方向不限。
func inWindow(ts, from, to time.Time) bool {
	if !from.IsZero() && ts.Before(from) {
		return false
	}
	if !to.IsZero() && ts.After(to) {
		return false
	}
	return true
}

// sortEvents 按 TS 升序排序；TS 相同时按 run_id、再按 seq 升序，保证结果确定。
func sortEvents(evs []events.Event) {
	sort.Slice(evs, func(i, j int) bool {
		a, b := evs[i], evs[j]
		if !a.TS.Equal(b.TS) {
			return a.TS.Before(b.TS)
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		return a.Seq < b.Seq
	})
}

// CountGaps 统计每个 run_id 内相邻 seq 的缺口总数（seq 差 - 1 之和）。
// 事件先按 run_id 分组、组内按 seq 排序再算，因此输入乱序不影响结果。
func CountGaps(evs []events.Event) int {
	if len(evs) < 2 {
		return 0
	}
	byRun := make(map[string][]uint64, 4)
	for i := range evs {
		byRun[evs[i].RunID] = append(byRun[evs[i].RunID], evs[i].Seq)
	}
	gaps := 0
	for _, seqs := range byRun {
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for i := 1; i < len(seqs); i++ {
			if diff := seqs[i] - seqs[i-1]; diff > 1 {
				gaps += int(diff - 1)
			}
		}
	}
	return gaps
}
