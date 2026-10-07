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
	gate        Gate                         // 暂停闸门；nil = 不设闸（老行为）。须在 Start 之前 SetGate
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

// Gate 暂停闸门。worker 在每次取任务前探一次；nil 表示不设闸（既有行为）。
//
// 闸门为什么放在**取任务循环顶部**，另外两个候选为什么不行：
//   - 卡 Submit：队列里已经躺着的那几百条会先跑完才停得下来，慢；而且调用方会把
//     "入队失败"翻译成"溢出到 DB"，语义混淆（任务被标成溢出，其实是被闸住了）。
//   - 卡 claimTask：那时任务已经被 worker 从 channel 里取走了，闸住就得把它塞回去 ——
//     channel 塞不回去（可能已满，塞回去还会打乱顺序）。
//
// 放在循环顶部则：队列原封不动（一条不丢）、在途任务自然跑完（不腰斩）、恢复瞬时
// （就是放行而已）、claimTask 完全不用改。队列长度也停在原地，后台能直接看到"积压 N 条"。
//
// **职责边界：只管运行期「要不要取下一个任务」，不管停机。** 收到停机信号后 worker
// 照旧把队列排空（见 drainAndExit）—— 停机是人为的、明确的终止，与"别再去打目标站"
// 是两回事。这里一度加过「暂停中不排空」，但那是**错配**的：熔断的暂停不跨重启，
// 重启后启动恢复会把同一批 pending 原样捞回来重跑，净效果为零，只多绕一趟 DB。
type Gate interface {
	// Wait 阻塞到放行或 stop 关闭；返回 true 表示继续干活，false 表示该停机了。
	//
	// 只有一个方法是有意的：池子问闸门的问题只有这一个。闸门的"现在是不是暂停态"
	// 归闸门自己（比如 breaker.Status()）或后台去过问，池子不需要知道 ——
	// 需要它的时候（暂停中不排空）才会有 Paused() 进来，而那条路已经拆了。
	Wait(stop <-chan struct{}) bool
}

// SetGate 设置暂停闸门。**必须在 Start 之前调用** —— worker 直接读这个字段，Start 之后再改就是数据竞争。
func (p *WorkerPool[T]) SetGate(g Gate) {
	p.gate = g
}

// waitGate 探一次闸门。没设闸或已放行返回 true。
func (p *WorkerPool[T]) waitGate() bool {
	if p.gate == nil {
		return true
	}
	return p.gate.Wait(p.stopCh)
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
		// 闸门优先于取任务：被闸住时一条都不消费，队列原封不动躺着。
		// stopCh 的优先级也天然对 —— Wait 里 select 的就是它，停机不会被暂停挡住。
		if !p.waitGate() {
			p.drainAndExit(ctx, workerID, handler)
			return
		}

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
//
// **不看闸门**：闸门管的是运行期「要不要取下一个任务」，停机是人为的、明确的终止，
// 不在它的职责里（理由见 Gate 的注释）。
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
// 返回 drained 表示是否**真的没有没跑完的任务**（判据是 submitted-completed-failed == 0）。
// 没排空时 inFlight 是那一刻尚未完成的任务数（含仍排在队列里、一次都没被取走的）。
//
// 这个返回值是给调用方做关停决策用的：只有 inFlight == 0 时才能确定没有任务正在持着
// 数据库连接/浏览器，这时候关库、关浏览器池才是安全的。反过来说，急着关会让在途任务
// 剩下的每一次写入都撞 "sql: database is closed" —— 等于把「让在途任务跑完」又亲手掐断。
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
		case <-time.After(timeout):
		}
		// drained 的判据是「还有没有没跑完的任务」，而不是「worker 退没退出」——
		// 上层拿它决定能不能关库/关浏览器池，那取决于前者。超时那一刻 worker 还在跑，
		// 两个判据才会分家。
		inFlight := int(p.submitted.Load() - p.completed.Load() - p.failed.Load())
		p.stopRes = stopResult{drained: inFlight == 0, inFlight: inFlight}
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
