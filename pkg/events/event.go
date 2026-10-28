// Package events 定义 papa 的 JSONL 事件契约与有界异步写入。
//
// 事件流是给机器消费的旁路数据：字段只增不改，读者必须忽略未知键。
// 本包只依赖标准库，不含任何具体业务语义。
package events

import "time"

// Version 是事件 schema 版本；只增不改。
const Version = 1

// 事件类型（稳定契约，只增不改）。
const (
	TypeRunStarted     = "run_started"
	TypeRunStopped     = "run_stopped"
	TypeTaskClaimed    = "task_claimed"
	TypeTaskFinished   = "task_finished"
	TypeTaskFailed     = "task_failed"
	TypeTaskRetry      = "task_retry_scheduled"
	TypeTaskRequeued   = "task_requeued"
	TypeQueueRound     = "queue_round"
	TypeBacklogSampled = "backlog_sampled"
	TypeStageSnapshot  = "stage_snapshot"
	TypeSpillChanged   = "spill_changed"
	TypeBreakerTripped = "breaker_tripped"
	TypePauseResumed   = "pause_resumed"
	TypeStagePool      = "stage_pool"
)

// Event 是事件流的一行；字段只增不改，读者必须忽略未知键。
// 除 v/seq/ts/run_id/type 外，零值字段靠 omitempty 省略，避免每行携带大量空键。
type Event struct {
	V           int            `json:"v"`
	Seq         uint64         `json:"seq"`
	TS          time.Time      `json:"ts"`
	RunID       string         `json:"run_id"`
	Type        string         `json:"type"`
	Site        string         `json:"site,omitempty"`
	Stage       string         `json:"stage,omitempty"`
	TaskID      uint           `json:"task_id,omitempty"`
	URL         string         `json:"url,omitempty"`
	Attempt     int            `json:"attempt,omitempty"`
	MaxAttempts int            `json:"max_attempts,omitempty"`
	Reprocess   int            `json:"reprocess,omitempty"`
	Worker      int            `json:"worker,omitempty"`
	Kind        string         `json:"kind,omitempty"`
	Outcome     string         `json:"outcome,omitempty"`
	DurationMS  int64          `json:"duration_ms,omitempty"`
	QueueWaitMS int64          `json:"queue_wait_ms,omitempty"`
	Queue       string         `json:"queue,omitempty"`
	Counts      map[string]int `json:"counts,omitempty"`
	Backlog     int64          `json:"backlog,omitempty"`
	Source      string         `json:"source,omitempty"`
	Error       string         `json:"error,omitempty"`
	// Attrs 是业务自定义的键值对（框架不解释其语义）：业务事件需要携带
	// `owner_type` / `owner_id` 这类框架不认识的维度时放这里，保持 Event 本身业务无关。
	Attrs map[string]string `json:"attrs,omitempty"`
	// 快照事件专用（stage_snapshot）。
	Stages map[string]StageSnapshot `json:"stages,omitempty"`
	Queues map[string]QueueSnapshot `json:"queues,omitempty"`
}

// StageSnapshot 是某阶段某一时刻的计数快照。
type StageSnapshot struct {
	Site       string `json:"site"`
	Stage      string `json:"stage"`
	QueueLen   int    `json:"queue_len"`
	InProgress int    `json:"in_progress"`
	Workers    int    `json:"workers"`
	Submitted  int64  `json:"submitted"`
	Completed  int64  `json:"completed"`
	Failed     int64  `json:"failed"`
}

// QueueSnapshot 是某治理队列某一时刻的计数快照。
type QueueSnapshot struct {
	Enabled        bool   `json:"enabled"`
	Running        bool   `json:"running"`
	TotalProcessed int64  `json:"total_processed"`
	RunProcessed   int64  `json:"run_processed"`
	Backlog        int64  `json:"backlog"`
	LastDurationMS int64  `json:"last_duration_ms"`
	LastError      string `json:"last_error,omitempty"`
}

const maxErrorLen = 2048

// TruncateError 把错误文本截到上限（事件里不含整段栈）。
func TruncateError(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	return s[:maxErrorLen]
}
