package workerpool

import (
	"context"
)

// Tasker 任务接口：唯一要求是可去重（Unique）。
type Tasker interface {
	Unique() string
}

// TaskHandler 处理单个任务
type TaskHandler[T Tasker] func(ctx context.Context, task T) error
