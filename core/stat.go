package core

import "time"

// 本文件是各处的**监控快照 DTO**：引擎往外吐的只读纯值类型。
// 抽到这里是为了让 internal/server 不必 import 引擎包（见包注释）。

// StageStats 阶段统计快照（纯值类型，供监控页读取，不暴露内部实现）
type StageStats struct {
	Global  GlobalStats
	Workers map[int]WorkerStat
	Queue   QueueStats
}

// GlobalStats 阶段全局统计
type GlobalStats struct {
	TotalTasks  int64
	TotalFailed int64
	TotalTime   time.Duration
	AvgTime     time.Duration
	MaxTime     time.Duration
	MinTime     time.Duration
}

// WorkerStat 单个 worker 统计
type WorkerStat struct {
	WorkerID    int
	TotalTasks  int64
	FailedTasks int64
	TotalTime   time.Duration
	MaxTime     time.Duration
	MinTime     time.Duration
}

// QueueStats 阶段队列计数
type QueueStats struct {
	Submitted  int64
	Completed  int64
	Failed     int64
	InProgress int64
	QueueLen   int
}

// StopStats 关停超时那一刻，某个阶段的存留情况。
type StopStats struct {
	Unfinished int // 提交了但没跑完的任务数（含仍在队列里排队的）
	Queued     int // 其中还没被 worker 取走、仍排在队列里的
}

// QueueStat 单个治理队列的运行快照（纯值类型，供监控页读取，不暴露内部实现）。
type QueueStat struct {
	Name           string        `json:"name"`
	Enabled        bool          `json:"enabled"`         // 是否启用自动轮询
	Running        bool          `json:"running"`         // 本轮是否正在执行
	Runs           int64         `json:"runs"`            // 累计执行次数
	StartedAt      time.Time     `json:"started_at"`      // 本轮开始时间（Running 时有意义）
	LastFinishAt   time.Time     `json:"last_finish_at"`  // 上次执行完成时间
	LastDuration   time.Duration `json:"last_duration"`   // 上次执行耗时
	LastProcessed  int           `json:"last_processed"`  // 上次实际重新投递的任务数
	RunProcessed   int64         `json:"run_processed"`   // 本轮已重新投递的任务数（Running 时递增）
	TotalProcessed int64         `json:"total_processed"` // 累计重新投递的任务数
	Backlog        int           `json:"backlog"`         // 待处理积压数（最近一次采样值）
	BacklogAt      time.Time     `json:"backlog_at"`      // 积压采样时间
	LastError      string        `json:"last_error"`      // 上次执行错误，空=正常
}

// TraceStep 一条步骤记录的快照（纯值类型，供监控后台读取，不暴露内部实现）。
type TraceStep struct {
	Attempt   int           `json:"attempt"`
	Seq       int           `json:"seq"`
	Step      string        `json:"step"`
	Status    string        `json:"status"` // ok / failed
	Kind      string        `json:"kind"`
	Message   string        `json:"message"`
	Data      string        `json:"data"` // 原始 JSON 文本，前端自行格式化
	Duration  time.Duration `json:"duration"`
	CreatedAt time.Time     `json:"created_at"`
}
