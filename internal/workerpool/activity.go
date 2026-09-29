package workerpool

import "time"

// ActivityType 活动类型//
type ActivityType int

const (
	ActivityTaskStart ActivityType = iota
	ActivityTaskEnd
	ActivityWorkerStop
)

// Activity 活动事件
type Activity struct {
	Type      ActivityType
	WorkerID  int
	Task      Tasker
	StartTime time.Time
	EndTime   time.Time
	Duration  time.Duration
	Error     error
}
