package eventlog

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
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
	res := Result{Files: len(paths)}
	seen := make(map[eventKey]struct{})
	for _, path := range paths {
		if err := readFile(path, from, to, seen, &res); err != nil {
			return Result{}, err
		}
	}
	sortEvents(res.Events)
	res.Gaps = countGaps(res.Events)
	return res, nil
}

// readFile 读取并解析一个 JSONL 文件，把结果合并进 res。
func readFile(path string, from, to time.Time, seen map[eventKey]struct{}, res *Result) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("eventlog: open %q: %w", path, err)
	}
	defer f.Close()

	var r io.Reader = f
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("eventlog: open gzip %q: %w", path, err)
		}
		defer zr.Close()
		r = zr
	}

	// 用 bufio.Reader 按行读取而不是 bufio.Scanner：事件行可能很长（错误文本等），
	// Scanner 的 token 上限会把长行变成读错误。
	br := bufio.NewReader(r)
	for {
		line, readErr := br.ReadString('\n')
		parseLine(line, from, to, seen, res)
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return fmt.Errorf("eventlog: read %q: %w", path, readErr)
		}
	}
}

// parseLine 解析一行 JSONL：空白行忽略，解析失败计入 Unparsed，窗口外丢弃，
// 重复 (run_id, seq) 计入 Duplicates，其余追加到 res.Events。
func parseLine(line string, from, to time.Time, seen map[eventKey]struct{}, res *Result) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var ev events.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		res.Unparsed++
		return
	}
	if !inWindow(ev.TS, from, to) {
		return
	}
	key := eventKey{runID: ev.RunID, seq: ev.Seq}
	if _, ok := seen[key]; ok {
		res.Duplicates++
		return
	}
	seen[key] = struct{}{}
	res.Events = append(res.Events, ev)
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

// countGaps 统计每个 run_id 内相邻 seq 的缺口总数（seq 差 - 1 之和）。
// 事件先按 run_id 分组、组内按 seq 排序再算，因此输入乱序不影响结果。
func countGaps(evs []events.Event) int {
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
