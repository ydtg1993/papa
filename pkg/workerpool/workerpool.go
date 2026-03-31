package workerpool

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
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
	logger     *logrus.Logger
}

// NewWorkerPool 创建工作池
func NewWorkerPool[T Tasker](workers, queueSize int, logger *logrus.Logger) *WorkerPool[T] {
	return &WorkerPool[T]{
		taskQueue:  make(chan T, queueSize),
		workers:    workers,
		errors:     make(chan error, queueSize),
		activities: make(chan Activity, workers*queueSize*2),
		logger:     logger,
	}
}

// Start 启动 worker 协程
func (p *WorkerPool[T]) Start(ctx context.Context, handler TaskHandler[T]) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(workerID int) {
			defer p.wg.Done()
			for task := range p.taskQueue {
				p.processTask(ctx, workerID, task, handler)
			}
			p.logger.Errorf("worker %d stopped", workerID)
		}(i)
	}
}

func (p *WorkerPool[T]) processTask(ctx context.Context, workerID int, task T, handler TaskHandler[T]) {
	start := time.Now()
	p.sendActivity(Activity{
		Type:      ActivityTaskStart,
		WorkerID:  workerID,
		Task:      task,
		StartTime: start,
	})

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
		p.sendError(fmt.Errorf("worker %d: %w", workerID, err))
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

// sendError非阻塞发送错误，失败时记录日志
func (p *WorkerPool[T]) sendError(err error) {
	select {
	case p.errors <- err:
	default:
		p.logger.Errorf("error dropped: %v", err)
	}
}

// Submit 提交任务，若已停止则拒绝
func (p *WorkerPool[T]) Submit(task T) error {
	if p.stopped.Load() {
		return fmt.Errorf("worker pool already stopped,failed to submit task: %v", task)
	}
	select {
	case p.taskQueue <- task:
		p.submitted.Add(1) // 提交成功，增加计数
		return nil
	default:
		return fmt.Errorf("task submission task url: %s", task.GetUrl())
	}
}

// Stop 优雅停止：不再接受新任务，等待所有 worker 完成（超时强制退出）
func (p *WorkerPool[T]) Stop(timeout time.Duration) {
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
			p.sendError(fmt.Errorf("all workers finished gracefully"))
		case <-time.After(timeout):
			p.sendError(fmt.Errorf("graceful stop timeout"))
		}
	})
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
