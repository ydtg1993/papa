package workerpool

import "context"

// Tasker 任务接口 方法
type Tasker interface {
	GetUrl() string
	GetRetry() int
	IncRetry()
}

// TaskHandler 处理单个任务
type TaskHandler[T Tasker] func(ctx context.Context, task T) error
