// Package eventlog 提供 papa 事件流文件（events.jsonl）的只读读取能力。
//
// 事件流契约见 pkg/events：一行一个 JSON 对象，字段只增不改、读者忽略未知键。
// 本包负责把磁盘上的事件流变成上层观测引擎可直接消费的一组事件与统计量：
//
//   - Discover：由主文件路径推出 lumberjack 的轮转备份（含 .gz 压缩备份）；
//   - Read：读取 JSONL（含 .gz）、按时间窗口过滤、按 (run_id, seq) 去重并排序；
//   - 统计：窗口内事件数、seq 缺口、坏行、重复条数。
//
// 本包只读、只依赖标准库与 pkg/events，不含任何具体业务语义。
package eventlog

import "github.com/ydtg1993/papa/v3/pkg/events"

// Result 是一次 Read 的统计结果。
type Result struct {
	// Events 是窗口内的事件，按 TS 升序排列；TS 相同时按 run_id、再按 seq 升序。
	Events []events.Event
	// Gaps 是同一 run_id 内相邻 seq 的缺口总数（相邻 seq 差 - 1 之和）。
	// 只在 Result.Events 之内的相邻记录之间计算；不同 run_id 之间不产生缺口。
	Gaps int
	// Files 是实际读取的文件个数（等于传入的路径数；读取失败时 Result 为零值）。
	Files int
	// Unparsed 是解析失败的 JSONL 行数。空行（只有空白字符）被忽略、不计入。
	Unparsed int
	// Duplicates 是按 (run_id, seq) 去重时丢弃的条数。
	Duplicates int
}
