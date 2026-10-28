// Package logidx 把人类可读的 logfmt 日志行解析成结构化证据条目，并按消息签名聚类。
//
// 它只做证据：把「日志文件里到底出现过什么」整理成可枚举、可比较的条目，不计算吞吐、
// 耗时、失败率这类指标，也不做采样推断 —— 指标口径应由调用方基于条目自行决定。
//
// 输入是 logrus TextFormatter 在文件输出（非终端）下写出的行：
//
//	time="2006-01-02 15:04:05" level=info msg="..." key=value key="a b"
//
// 时间戳本身不带时区，解析位置由 Config.Loc 决定（nil = UTC）。
package logidx

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 默认值与上限。
const (
	defaultMaxEntries = 20000
	defaultMaxSamples = 3
)

// Entry 一条解析成功的日志记录。
type Entry struct {
	File   string            // 文件名（不含目录）
	Line   int               // 1 起
	Time   time.Time         // time 字段按 Config.Loc 解析出的时刻
	Level  string            // info/warning/error/debug...（原样保留，小写是 logrus 的写法）
	Logger string            // 由文件名推出：engine.log -> engine（.gz 同样剥掉）
	Msg    string            // msg 字段，转义已还原（可能含换行/引号）
	Fields map[string]string // 除 time/level/msg 之外的字段；没有额外字段时为 nil
	Raw    string            // 原文（按字节截断到 2KB，保证 UTF-8 边界）
}

// Signature 一条消息签名及其聚合结果。
type Signature struct {
	Pattern string    // 掩码后的签名：URL-><url>、长 hex/UUID-><id>、数字->#，再截断到 120 字符
	Count   int       // 命中条数
	First   time.Time // 最早出现的时刻
	Last    time.Time // 最晚出现的时刻
	Samples []Entry   // 最早出现的至多 MaxSamples 条，按时刻升序
}

// Config Build 的输入。
type Config struct {
	Paths      []string       // 日志文件路径；支持 .gz 压缩文件
	From       time.Time      // 窗口下界（含），零值 = 不限
	To         time.Time      // 窗口上界（含），零值 = 不限
	Loc        *time.Location // 解析无时区时间戳用；nil = UTC
	MaxEntries int            // 最多接收多少条记录；0 = 20000
	MaxSamples int            // 每条签名保留多少条样例；0 = 3
}

// Index Build 的结果。
type Index struct {
	Signatures []Signature // Count 降序，同 Count 按 Pattern 字典序
	Files      int         // 实际读取的文件数
	Unparsed   int         // 无法解析（或不带可解析 time/level）的非空行数
	Truncated  bool        // 是否因达到 MaxEntries 提前停止读取
}

// Build 读取 Config.Paths 并按消息签名聚合。
//
// 行为约定：
//   - 空行跳过，不计入 Unparsed（文件末尾的换行不算记录）；
//   - 一行解析失败只累加 Unparsed，不影响其它行；
//   - 窗口按闭区间 [From, To] 判定，零值边界表示该侧不限；
//   - 接收满 MaxEntries 条后立刻停止读取（剩余文件和行不再处理），并置 Truncated；
//     恰好等于 MaxEntries 而没有更多记录时 Truncated 为 false；
//   - 任一文件打不开/读失败时返回错误（Index 为 nil）。
//
// 两次相同输入的 Build 结果可 DeepEqual：签名排序、样例顺序都有确定的 tie-break。
func Build(cfg Config) (*Index, error) {
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	maxSamples := cfg.MaxSamples
	if maxSamples <= 0 {
		maxSamples = defaultMaxSamples
	}
	loc := cfg.Loc
	if loc == nil {
		loc = time.UTC
	}

	idx := &Index{}
	byPattern := make(map[string]*Signature)
	accepted := 0
	stop := false

	for _, path := range cfg.Paths {
		if stop {
			break
		}
		idx.Files++
		unparsed, err := readEntries(path, loc, func(e Entry) bool {
			if !withinWindow(e.Time, cfg.From, cfg.To) {
				return true
			}
			if accepted >= maxEntries {
				idx.Truncated = true
				stop = true
				return false
			}
			accepted++
			pattern := signatureOf(e.Msg)
			sig, ok := byPattern[pattern]
			if !ok {
				sig = &Signature{Pattern: pattern}
				byPattern[pattern] = sig
			}
			sig.add(e, maxSamples)
			return true
		})
		idx.Unparsed += unparsed
		if err != nil {
			return nil, fmt.Errorf("logidx: read %q: %w", path, err)
		}
	}

	idx.Signatures = make([]Signature, 0, len(byPattern))
	for _, sig := range byPattern {
		idx.Signatures = append(idx.Signatures, *sig)
	}
	sort.Slice(idx.Signatures, func(i, j int) bool {
		if idx.Signatures[i].Count != idx.Signatures[j].Count {
			return idx.Signatures[i].Count > idx.Signatures[j].Count
		}
		return idx.Signatures[i].Pattern < idx.Signatures[j].Pattern
	})
	return idx, nil
}

// add 把一条记录并入签名：更新计数与首末时刻，并保留最早的 maxSamples 条样例。
func (s *Signature) add(e Entry, maxSamples int) {
	s.Count++
	if s.Count == 1 || e.Time.Before(s.First) {
		s.First = e.Time
	}
	if s.Count == 1 || e.Time.After(s.Last) {
		s.Last = e.Time
	}
	s.Samples = append(s.Samples, e)
	sort.SliceStable(s.Samples, func(i, j int) bool { return sampleLess(s.Samples[i], s.Samples[j]) })
	if len(s.Samples) > maxSamples {
		s.Samples = s.Samples[:maxSamples]
	}
}

// sampleLess 给样例排序：时刻升序，同刻按文件、行号升序（保证确定性）。
func sampleLess(a, b Entry) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.Before(b.Time)
	}
	if a.File != b.File {
		return a.File < b.File
	}
	return a.Line < b.Line
}

// withinWindow 判断时刻是否落在闭区间内；零值边界表示该侧不限。
func withinWindow(t, from, to time.Time) bool {
	if !from.IsZero() && t.Before(from) {
		return false
	}
	if !to.IsZero() && t.After(to) {
		return false
	}
	return true
}

// readEntries 读取一个文件（可选 gzip），把解析成功的记录交给 yield。
// yield 返回 false 表示调用方不要再喂了，函数立即返回。
// 返回值为未能解析的非空行数。
func readEntries(path string, loc *time.Location, yield func(Entry) bool) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var reader io.Reader = f
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		gz, gerr := gzip.NewReader(f)
		if gerr != nil {
			return 0, gerr
		}
		defer gz.Close()
		reader = gz
	}

	br := bufio.NewReader(reader)
	file := filepath.Base(path)
	logger := loggerName(file)
	unparsed := 0
	lineNo := 0

	for {
		raw, readErr := br.ReadString('\n')
		if raw != "" {
			lineNo++
			text := strings.TrimSuffix(raw, "\n")
			text = strings.TrimSuffix(text, "\r")
			if strings.TrimSpace(text) != "" {
				if entry, ok := parseLine(text, loc, file, logger, lineNo); ok {
					if !yield(entry) {
						return unparsed, nil
					}
				} else {
					unparsed++
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return unparsed, readErr
		}
	}
	return unparsed, nil
}
