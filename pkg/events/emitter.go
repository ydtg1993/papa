package events

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// defaultBuffer 是 buffer <= 0 时的默认队列长度（约 8192 条事件）。
const defaultBuffer = 8192

// Emitter 接收事件；NopEmitter 是关闭事件流时的默认实现，调用零分配。
type Emitter interface {
	Emit(Event)
}

// Closer 是可关闭的 Emitter：停机时先用 Emit 写收尾事件，再限时排空并关闭 sink。
type Closer interface {
	Emitter
	Close(ctx context.Context) error
	Dropped() uint64
}

// NopEmitter 丢弃所有事件。
type NopEmitter struct{}

// Emit 什么都不做。
func (NopEmitter) Emit(Event) {}

// bufferedEmitter 有界异步写入：worker 永不阻塞，满了丢弃并计数。
type bufferedEmitter struct {
	runID   string
	sink    *JSONLSink
	ch      chan Event
	seq     atomic.Uint64
	dropped atomic.Uint64
	now     func() time.Time

	// mu/closed 让 Close 之后的 Emit 安全：Emit 只在读锁内入队，Close 在写锁内关 channel，
	// 因此不存在向已关闭 channel 发送的窗口。
	mu     sync.RWMutex
	closed bool

	done chan struct{} // drain 退出后关闭
}

var (
	_ Emitter = NopEmitter{}
	_ Closer  = (*bufferedEmitter)(nil)
)

// NewEmitter 构造一个有界异步 emitter 并启动后台 drain。
// buffer <= 0 时用 defaultBuffer；now 为 nil 时用 time.Now；sink 不能为 nil。
func NewEmitter(runID string, sink *JSONLSink, buffer int, now func() time.Time) *bufferedEmitter {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	if now == nil {
		now = time.Now
	}
	e := &bufferedEmitter{
		runID: runID,
		sink:  sink,
		ch:    make(chan Event, buffer),
		now:   now,
		done:  make(chan struct{}),
	}
	go e.drain()
	return e
}

// Emit 非阻塞：补全 v/run_id/ts/seq 后入队，buffer 满则丢弃并计数。
// Close 之后调用同样安全（只计 dropped，不 panic）。
func (e *bufferedEmitter) Emit(ev Event) {
	ev.V = Version
	ev.RunID = e.runID
	if ev.TS.IsZero() {
		ev.TS = e.now()
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		e.dropped.Add(1)
		return
	}
	ev.Seq = e.seq.Add(1)
	select {
	case e.ch <- ev:
	default:
		e.dropped.Add(1)
	}
}

// drain 串行消费队列；写失败也计入 dropped，避免静默丢事件。
func (e *bufferedEmitter) drain() {
	for ev := range e.ch {
		if err := e.sink.Write(ev); err != nil {
			e.dropped.Add(1)
		}
	}
	close(e.done)
}

// Dropped 返回被丢弃的事件数（buffer 满、写失败或 Close 排空超时）。
func (e *bufferedEmitter) Dropped() uint64 { return e.dropped.Load() }

// Close 关闭入口并限时排空：先置 closed 并 close(ch)，再在锁外等 drain 完成，
// 完成后再关 sink（避免与最后一次 Write 并发）。ctx 超时则计入 dropped 并返回 ctx.Err()，
// 此时 sink 不关闭（drain 仍可能在写），由调用方决定后续清理。
// 重复调用安全；并发的多次调用各自等待 drain，sink.Close 由 JSONLSink 保证幂等。
func (e *bufferedEmitter) Close(ctx context.Context) error {
	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.ch)
	}
	e.mu.Unlock()

	select {
	case <-e.done:
		return e.sink.Close()
	case <-ctx.Done():
		// 排空超时：队列里还剩多少条无法精确统计，计入一次丢失。
		e.dropped.Add(1)
		return ctx.Err()
	}
}
