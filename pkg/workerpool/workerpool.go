package workerpool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// WorkerPool 泛型工作池
type WorkerPool[T Tasker] struct {
	taskQueue  chan T
	workers    int
	wg         sync.WaitGroup
	stopOnce   sync.Once
	stopped    atomic.Bool
	errors     chan error
	cancel     context.CancelFunc
	submitted  atomic.Int64 // 已提交的任务总数
	completed  atomic.Int64 // 已完成的任务数
	failed     atomic.Int64 // 失败的任务数（可选）
	activities chan Activity
}

// Tasker 任务接口 方法
type Tasker interface {
	GetUrl() string
	GetRetry() int
	IncRetry()
}

// TaskHandler 处理单个任务
type TaskHandler[T Tasker] func(ctx context.Context, task T) error

// ActivityType 活动类型//
type ActivityType int

const (
	ActivityTaskStart ActivityType = iota
	ActivityTaskEnd
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

// NewWorkerPool 创建工作池
func NewWorkerPool[T Tasker](workers, queueSize int) *WorkerPool[T] {
	return &WorkerPool[T]{
		taskQueue:  make(chan T, queueSize),
		workers:    workers,
		errors:     make(chan error, queueSize),
		activities: make(chan Activity, workers*queueSize),
	}
}

// Start 启动 worker 协程
func (p *WorkerPool[T]) Start(handler TaskHandler[T]) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(workerID int) {
			defer p.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					p.errors <- fmt.Errorf("worker %d panic: %v", workerID, r)
				}
			}()
			for task := range p.taskQueue {
				p.processTask(workerID, task, handler)
			}
			p.errors <- fmt.Errorf("worker %d stopped", workerID)
		}(i)
	}
}

func (p *WorkerPool[T]) processTask(workerID int, task T, handler TaskHandler[T]) {
	start := time.Now()
	p.sendActivity(Activity{
		Type:      ActivityTaskStart,
		WorkerID:  workerID,
		Task:      task,
		StartTime: start,
	})

	ctx := context.Background()
	err := handler(ctx, task)

	p.sendActivity(Activity{
		Type:      ActivityTaskEnd,
		WorkerID:  workerID,
		Task:      task,
		StartTime: start,
		EndTime:   time.Now(),
		Duration:  time.Since(start),
		Error:     err,
	})

	if err != nil {
		p.failed.Add(1)
		p.errors <- fmt.Errorf("worker %d: %w", workerID, err)
	} else {
		p.completed.Add(1)
	}
}

func (p *WorkerPool[T]) sendActivity(act Activity) {
	select {
	case p.activities <- act:
	default:
		p.errors <- fmt.Errorf("activity dropped")
	}
}

// Submit 提交任务，若已停止则拒绝
func (p *WorkerPool[T]) Submit(task T) error {
	if p.stopped.Load() {
		return fmt.Errorf("worker pool already stopped")
	}
	select {
	case p.taskQueue <- task:
		p.submitted.Add(1) // 提交成功，增加计数
		return nil
	case <-time.After(30 * time.Second):
		return fmt.Errorf("task submission timeout")
	}
}

// Stop 优雅停止：不再接受新任务，等待所有 worker 完成（超时强制退出）
func (p *WorkerPool[T]) Stop(timeout time.Duration) error {
	var err error
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		close(p.taskQueue) // 不再接收新任务
		done := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			p.errors <- fmt.Errorf("all workers finished gracefully")
		case <-time.After(timeout):
			p.errors <- fmt.Errorf("graceful stop timeout, forcing exit")
			err = fmt.Errorf("graceful stop timeout after %v", timeout)
		}
		//close(p.errors)
		close(p.activities)
	})
	return err
}

// Errors 返回错误通道
func (p *WorkerPool[T]) Errors() <-chan error {
	return p.errors
}

func (p *WorkerPool[T]) Activities() <-chan Activity {
	return p.activities
}

// Stats 返回当前池的统计信息
// Stats 返回统计信息
func (p *WorkerPool[T]) Stats() (submitted, completed, failed, inProgress int64, queueLen int) {
	submitted = p.submitted.Load()
	completed = p.completed.Load()
	failed = p.failed.Load()
	inProgress = submitted - completed - failed
	queueLen = len(p.taskQueue)
	return
}
