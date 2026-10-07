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

// urgentMinQueueSize 快车道的容量下限；实际容量取 max(workers*2, urgentMinQueueSize)。
// 容量必须小：快车道饱和时主队列会被饿住（这是快车道固有的代价），小容量把它限制在可接受的量级。
const urgentMinQueueSize = 8

// WorkerPool 泛型工作池。
//
// 两条队列：taskQueue 是常规通道，urgentQueue 是"快车道"（见 SubmitUrgent）。
// 两条队列**从不 close** —— 停机靠 stopCh 信号，Submit 侧靠 stopped + 读写锁互斥，
// 于是"向已关闭 channel 发送"这一类隐患在结构上不存在。
type WorkerPool[T Tasker] struct {
	taskQueue   chan T
	urgentQueue chan T
	workers     int
	watermark   float64 // 队列高水位比例(0-1)，达到后 Submit 返回 ErrQueueFull
	wg          sync.WaitGroup
	stopOnce    sync.Once
	stopRes     stopResult    // Stop 的结局；stopOnce 跑完后才有效，供重复调用返回同一份
	stopCh      chan struct{} // 停机信号；队列不 close，worker 靠它退出
	mu          sync.RWMutex  // 保护 stopped，并与 Submit 的发送互斥，见 Submit/Stop
	stopped     bool
	cancel      context.CancelFunc
	submitted   atomic.Int64                 // 已提交的任务总数
	completed   atomic.Int64                 // 已完成的任务数
	failed      atomic.Int64                 // 失败的任务数（可选）
	trackQueue  *msgqueue.MsgQueue[Activity] //系统消息队列
}

// NewWorkerPool 创建工作池；watermark 为队列高水位比例(0-1)，非法值回退 0.75。
func NewWorkerPool[T Tasker](workers, queueSize int, watermark float64) *WorkerPool[T] {
	if watermark <= 0 || watermark > 1 {
		watermark = 0.75
	}
	urgentSize := max(workers*2, urgentMinQueueSize)
	return &WorkerPool[T]{
		taskQueue:   make(chan T, queueSize),
		urgentQueue: make(chan T, urgentSize),
		workers:     workers,
		watermark:   watermark,
		stopCh:      make(chan struct{}),
		trackQueue:  msgqueue.NewMsgQueue[Activity](100),
	}
}

// Start 启动 worker 协程
func (p *WorkerPool[T]) Start(ctx context.Context, handler TaskHandler[T]) {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go func(workerID int) {
			defer p.wg.Done()
			p.run(ctx, workerID, handler)
			p.trackQueue.SendActivity(Activity{
				Type:     ActivityWorkerStop,
				WorkerID: workerID,
				EndTime:  time.Now(),
			})
		}(i)
	}
}

// run 单个 worker 的取任务循环。快车道优先：循环顶部先非阻塞看一眼快车道，
// 只要它非空就一定先取它 —— 不能写成双 case 的 select，Go 在多路同时就绪时是**随机**选，
// 那样快车道会退化成"另一条车道"。
//
// **故意没有 `case <-ctx.Done()`**：Engine.Stop 是先 cancel 再 pool.Stop，
// 进 Stop 时 ctx.Done() 早就关闭了；放进来 worker 会在停机瞬间随机选中它直接返回，
// 两条队列里的存量任务被丢掉（比旧的 range 语义严格更差）。
func (p *WorkerPool[T]) run(ctx context.Context, workerID int, handler TaskHandler[T]) {
	for {
		select {
		case task := <-p.urgentQueue:
			p.processTask(ctx, workerID, task, handler)
			continue
		default:
		}

		select {
		case task := <-p.urgentQueue:
			p.processTask(ctx, workerID, task, handler)
		case task := <-p.taskQueue:
			p.processTask(ctx, workerID, task, handler)
		case <-p.stopCh:
			p.drainAndExit(ctx, workerID, handler)
			return
		}
	}
}

// drainAndExit 停机后把两条队列里的存量跑完再退出，保持旧的 range 语义。
//
// 判定"空"时不会再有任务落进来：Submit/SubmitUrgent 全程持 RLock 完成"查 stopped + 发送"，
// Stop 取写锁后才置 stopped 并 close(stopCh)，两者互斥 —— close(stopCh) 之后不可能再有发送成功。
func (p *WorkerPool[T]) drainAndExit(ctx context.Context, workerID int, handler TaskHandler[T]) {
	for {
		select {
		case task := <-p.urgentQueue:
			p.processTask(ctx, workerID, task, handler)
		case task := <-p.taskQueue:
			p.processTask(ctx, workerID, task, handler)
		default:
			return
		}
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
// worker 的取任务循环也已经断了，该阶段会永久少一个 worker。转成 error 后按普通失败计数、上报。
func (p *WorkerPool[T]) runHandler(ctx context.Context, task T, handler TaskHandler[T]) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return handler(ctx, task)
}

// Submit 提交到常规队列，若已停止则拒绝；达到水位时返回 ErrQueueFull 由上层溢出。
func (p *WorkerPool[T]) Submit(task T) error {
	// 读锁覆盖「判断 stopped + 发送」整段：Stop 置位并 close(stopCh) 时持写锁，两者互斥。
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.submitLocked(task)
}

// SubmitUrgent 提交到快车道：让这一条插到所在队列的前面。
//
// 快车道满了并不报错，而是**退到常规队列**（best-effort 优先）。调用方把 ErrQueueFull
// 统一翻译成"溢出到 DB、每 2s 回灌一次"，对加急任务那等于把加急属性悄悄吃掉；
// 而快车道容量小（见 urgentMinQueueSize），早早拒收反而更糟。只有两条都满才回 ErrQueueFull。
func (p *WorkerPool[T]) SubmitUrgent(task T) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if !p.stopped && float64(len(p.urgentQueue)) < float64(cap(p.urgentQueue))*p.watermark {
		select {
		case p.urgentQueue <- task:
			p.submitted.Add(1)
			return nil
		default:
		}
	}
	return p.submitLocked(task)
}

// submitLocked 向常规队列发送；调用方须已持锁。
func (p *WorkerPool[T]) submitLocked(task T) error {
	if p.stopped {
		err := fmt.Errorf("worker pool already stopped,failed to submit task: %+v", task)
		p.trackQueue.SendError(err)
		return err
	}
	// 达到水位时提前拒绝入队，避免 100% 时静默丢弃，由上层把任务留库/溢出。
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

// stopResult 一次 Stop 的结局快照。存进池子里是为了让重复调用 Stop（stopOnce 只跑一次）
// 也拿到同一份结果，而不是零值。
type stopResult struct {
	drained  bool
	inFlight int
}

// Stop 优雅停止：不再接受新任务，等待所有 worker 把两条队列的存量跑完。
//
// 返回 drained 表示是否在 timeout 内排空。没排空时 inFlight 是那一刻**尚未完成**的任务数
// （= 提交过但既没完成也没失败，含仍排在队列里、一次都没被取走的）。
//
// 这个返回值是给调用方做关停决策用的：超时返回后 worker goroutine **还活着**（Go 杀不掉它），
// 这时候急着关数据库/关浏览器池，会让它们剩下的每一次写入都撞 "sql: database is closed" ——
// 等于把「让在途任务跑完」这件事又亲手掐断。
//
// 重复调用返回首次的结果。
func (p *WorkerPool[T]) Stop(timeout time.Duration) (drained bool, inFlight int) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		close(p.stopCh) // 通知 worker 停机；队列不 close，见 WorkerPool 的说明
		p.mu.Unlock()
		done := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			p.stopRes = stopResult{drained: true}
		case <-time.After(timeout):
			p.stopRes = stopResult{
				inFlight: int(p.submitted.Load() - p.completed.Load() - p.failed.Load()),
			}
		}
	})
	return p.stopRes.drained, p.stopRes.inFlight
}

// Stats 返回当前池的统计信息。queueLen 是两条队列长度之和。
func (p *WorkerPool[T]) Stats() (submitted, completed, failed, inProgress int64, queueLen int) {
	submitted = p.submitted.Load()
	completed = p.completed.Load()
	failed = p.failed.Load()
	inProgress = submitted - completed - failed
	main, urgent := p.QueueDepths()
	queueLen = main + urgent
	return
}

// QueueDepths 分别返回常规队列与快车道的当前长度。
func (p *WorkerPool[T]) QueueDepths() (main, urgent int) {
	return len(p.taskQueue), len(p.urgentQueue)
}

func (p *WorkerPool[T]) Activities() <-chan Activity {
	return p.trackQueue.Activities()
}

func (p *WorkerPool[T]) Errors() <-chan error {
	return p.trackQueue.Errors()
}
