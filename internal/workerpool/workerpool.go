package workerpool

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v2/internal/msgqueue"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// ErrQueueFull 队列达到高水位（75%），任务应溢出到数据库而非入队。
var ErrQueueFull = errors.New("worker pool queue full")

// WorkerPool 泛型工作池
type WorkerPool[T Tasker] struct {
	taskQueue  chan T
	workers    int
	watermark  float64 // 队列高水位比例(0-1)，达到后 Submit 返回 ErrQueueFull
	wg         sync.WaitGroup
	stopOnce   sync.Once
	mu         sync.RWMutex // 保护 stopped 与 taskQueue 的关闭，见 Submit/Stop
	stopped    bool
	cancel     context.CancelFunc
	submitted  atomic.Int64                 // 已提交的任务总数
	completed  atomic.Int64                 // 已完成的任务数
	failed     atomic.Int64                 // 失败的任务数（可选）
	trackQueue *msgqueue.MsgQueue[Activity] //系统消息队列
}

// NewWorkerPool 创建工作池；watermark 为队列高水位比例(0-1)，非法值回退 0.75。
func NewWorkerPool[T Tasker](workers, queueSize int, watermark float64) *WorkerPool[T] {
	if watermark <= 0 || watermark > 1 {
		watermark = 0.75
	}
	return &WorkerPool[T]{
		taskQueue:  make(chan T, queueSize),
		workers:    workers,
		watermark:  watermark,
		trackQueue: msgqueue.NewMsgQueue[Activity](10),
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

	err := p.runHandler(ctx, task, handler)

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

// runHandler 执行 handler，并把 panic 转成 error 返回。
// 单条任务 panic 不能逃出 worker goroutine：那会直接崩掉进程，即使外层补 recover，
// worker 的 for range 也已经断了，该阶段会永久少一个 worker。转成 error 后按普通失败计数、上报。
func (p *WorkerPool[T]) runHandler(ctx context.Context, task T, handler TaskHandler[T]) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return handler(ctx, task)
}

// Submit 提交任务，若已停止则拒绝；达到 75% 高水位时返回 ErrQueueFull 由上层溢出。
func (p *WorkerPool[T]) Submit(task T) error {
	// 读锁覆盖「判断 stopped + 发送」整段：Stop 置位并 close 时持写锁，两者互斥。
	// 只在锁外判断再发送的话，中间会被 Stop 的 close 插进来 → 向已关闭 channel 发送 panic。
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.stopped {
		err := fmt.Errorf("worker pool already stopped,failed to submit task: %+v", task)
		p.trackQueue.SendError(err)
		return err
	}
	// 达到高水位时提前拒绝入队，避免 100% 时静默丢弃，由上层把任务留库/溢出。
	if float64(len(p.taskQueue)) >= float64(cap(p.taskQueue))*p.watermark {
		return ErrQueueFull
	}
	select {
	case p.taskQueue <- task:
		p.submitted.Add(1) // 提交成功，增加计数
		return nil
	default:
		return ErrQueueFull
	}
}

// Stop 优雅停止：不再接受新任务，等待所有 worker 完成（超时强制退出）
func (p *WorkerPool[T]) Stop(timeout time.Duration) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		close(p.taskQueue) // 不再接收新任务；与 Submit 的发送互斥
		p.mu.Unlock()
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
