package workerpool

import (
	"context"
	"fmt"
	pkg2 "github.com/ydtg1993/papa/pkg"
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
	cancel     context.CancelFunc
	submitted  atomic.Int64             // 已提交的任务总数
	completed  atomic.Int64             // 已完成的任务数
	failed     atomic.Int64             // 失败的任务数（可选）
	trackQueue *pkg2.MsgQueue[Activity] //系统消息队列
}

// NewWorkerPool 创建工作池
func NewWorkerPool[T Tasker](workers, queueSize int) *WorkerPool[T] {
	return &WorkerPool[T]{
		taskQueue:  make(chan T, queueSize),
		workers:    workers,
		trackQueue: pkg2.NewMsgQueue[Activity](10),
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
			p.trackQueue.SendActivity(Activity{
				Type:     ActivityWorkerStop,
				WorkerID: workerID,
				EndTime:  time.Now(),
			})
		}(i)
	}
}

func (p *WorkerPool[T]) processTask(ctx context.Context, workerID int, task T, handler TaskHandler[T]) {
	start := time.Now()
	p.trackQueue.SendActivity(Activity{
		Type:      ActivityTaskStart,
		WorkerID:  workerID,
		Task:      task,
		StartTime: start,
	})

	err := handler(ctx, task)

	p.trackQueue.SendActivity(Activity{
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
		p.trackQueue.SendError(fmt.Errorf("worker %d: %w", workerID, err))
	} else {
		p.completed.Add(1)
	}
}

// Submit 提交任务，若已停止则拒绝
func (p *WorkerPool[T]) Submit(task T) error {
	if p.stopped.Load() {
		err := fmt.Errorf("worker pool already stopped,failed to submit task: %+v", task)
		p.trackQueue.SendError(err)
		return err
	}
	select {
	case p.taskQueue <- task:
		p.submitted.Add(1) // 提交成功，增加计数
		return nil
	default:
		err := fmt.Errorf("task submission task url: %s", task.GetUrl())
		p.trackQueue.SendError(err)
		return err
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
			p.trackQueue.SendError(fmt.Errorf("all workers finished gracefully"))
		case <-time.After(timeout):
			p.trackQueue.SendError(fmt.Errorf("graceful stop timeout"))
		}
	})
}

// Stats 返回当前池的统计信息
func (p *WorkerPool[T]) Stats() (submitted, completed, failed, inProgress int64, queueLen int) {
	submitted = p.submitted.Load()
	completed = p.completed.Load()
	failed = p.failed.Load()
	inProgress = submitted - completed - failed
	queueLen = len(p.taskQueue)
	return
}

func (p *WorkerPool[T]) Activities() <-chan Activity {
	return p.trackQueue.Activities()
}

func (p *WorkerPool[T]) Errors() <-chan error {
	return p.trackQueue.Errors()
}
