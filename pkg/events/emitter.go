package events

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const defaultBuffer = 256
const maxBuffer = 8192

type Emitter interface{ Emit(Event) }
type Closer interface {
	Emitter
	Close(context.Context) error
	Dropped() uint64
}
type NopEmitter struct{}

func (NopEmitter) Emit(Event)                  {}
func (NopEmitter) Close(context.Context) error { return nil }
func (NopEmitter) Dropped() uint64             { return 0 }

// bufferedEmitter owns its queue and a single consumer goroutine. No producer
// or closer invokes user code. A stuck callback cannot create replacement workers.
type bufferedEmitter struct {
	ch        chan Event
	seq       atomic.Uint64
	dropped   atomic.Uint64
	mu        sync.Mutex
	closed    bool
	done      chan struct{}
	err       error // written by consumer before done closes
	consume   func(Event) error
	finish    func() error
	stampTime bool // only the standard clock runs on producers
}

func newBuffered(buffer int, consume func(Event) error, finish func() error) *bufferedEmitter {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	if buffer > maxBuffer {
		buffer = maxBuffer
	}
	e := &bufferedEmitter{ch: make(chan Event, buffer), done: make(chan struct{}), consume: consume, finish: finish, stampTime: true}
	go e.drain()
	return e
}

// NewEmitter creates an independently bounded archive consumer. Clock and sink
// callbacks execute only in that consumer, including sink.Close. With nil now,
// missing timestamps are captured at admission. A custom clock is a consumer
// fallback; timestamps already captured by an outer Isolate are preserved.
func NewEmitter(runID string, sink *JSONLSink, buffer int, now func() time.Time) *bufferedEmitter {
	standardClock := now == nil
	if now == nil {
		now = time.Now
	}
	owned, _ := Detach(Event{RunID: runID})
	e := newBuffered(buffer, func(ev Event) error {
		ev.V = Version
		ev.RunID = owned.RunID
		ev.Truncated = ev.Truncated || owned.Truncated
		if ev.TS.IsZero() {
			ev.TS = now()
		}
		return sink.Write(ev)
	}, sink.Close)
	e.stampTime = standardClock
	return e
}

// Isolate wraps unknown emitters in one fixed queue and worker. Built-in safe
// emitters pass through. Call once during wiring, before execution begins.
func Isolate(em Emitter) Closer {
	if em == nil {
		return nil
	}
	switch e := em.(type) {
	case *bufferedEmitter:
		return e
	case NopEmitter:
		return e
	}
	return newBuffered(defaultBuffer, func(ev Event) error { em.Emit(ev); return nil }, func() error {
		if c, ok := em.(interface{ Close(context.Context) error }); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return c.Close(ctx)
		}
		return nil
	})
}

func (e *bufferedEmitter) Emit(ev Event) {
	if ev.TS.IsZero() && e.stampTime {
		ev.TS = time.Now()
	}
	ev, _ = Detach(ev)
	// Sequence assignment and admission must share one order even with multiple
	// producers. This lock never encloses copying, clock callbacks or sink I/O.
	e.mu.Lock()
	defer e.mu.Unlock()
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

func (e *bufferedEmitter) stopInput() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		close(e.ch)
	}
}

func protect(fn func() error) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("events: consumer panic")
		}
	}()
	return fn()
}

func (e *bufferedEmitter) drain() {
	defer close(e.done)
	// Panic disables this consumer and closes admission; queued data is dropped.
	func() {
		defer func() {
			if recover() != nil {
				e.err = errors.New("events: consumer panic")
				e.dropped.Add(1)
				e.stopInput()
				for range e.ch {
					e.dropped.Add(1)
				}
			}
		}()
		for ev := range e.ch {
			if err := e.consume(ev); err != nil {
				e.dropped.Add(1)
			}
		}
	}()
	if err := protect(e.finish); err != nil {
		e.err = err
		e.dropped.Add(1)
	}
}
func (e *bufferedEmitter) Dropped() uint64 { return e.dropped.Load() }

// Close closes admission immediately and bounds the entire wait, including the
// underlying Close callback. Repeated calls never create additional goroutines.
func (e *bufferedEmitter) Close(ctx context.Context) error {
	e.stopInput()
	select {
	case <-e.done:
		return e.err
	default:
	}
	select {
	case <-e.done:
		return e.err
	case <-ctx.Done():
		e.dropped.Add(1)
		return ctx.Err()
	}
}
