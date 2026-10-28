package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingSink 是测试用 io.WriteCloser：首次 Write 时关闭 entered，然后阻塞在 gate 上，
// 直到测试 close(gate)。用它可以确定性地知道 drain 已取走队列里的事件并卡在写盘，
// 从而保证后续 Emit 真的塞满 buffer，不依赖 goroutine 调度。
type blockingSink struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	entered   chan struct{}
	enterOnce sync.Once
	gate      chan struct{}
}

func newBlockingSink() *blockingSink {
	return &blockingSink{entered: make(chan struct{}), gate: make(chan struct{})}
}

func (s *blockingSink) Write(p []byte) (int, error) {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.gate
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *blockingSink) Close() error { return nil }

func (s *blockingSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *blockingSink) release() { close(s.gate) }

func TestEmitterAssignsMonotonicSeqAndRunID(t *testing.T) {
	var w bufCloser
	e := NewEmitter("r-9", NewJSONLSink(&w), 8, time.Now)
	e.Emit(Event{Type: TypeRunStarted})
	e.Emit(Event{Type: TypeRunStopped})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), w.String())
	}
	for i, line := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if ev.Seq != uint64(i+1) {
			t.Fatalf("line %d seq = %d, want %d", i, ev.Seq, i+1)
		}
		if ev.V != Version || ev.RunID != "r-9" {
			t.Fatalf("line %d v/run_id = %d/%q", i, ev.V, ev.RunID)
		}
		if ev.TS.IsZero() {
			t.Fatalf("line %d ts not injected", i)
		}
	}
	if got := e.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
}

func TestEmitterDropsWhenFull(t *testing.T) {
	sink := newBlockingSink()
	e := NewEmitter("r-drop", NewJSONLSink(sink), 1, time.Now)

	// 第一条会被 drain 取走并卡在 sink.Write 里：等到 entered 就说明 buffer 已空。
	e.Emit(Event{Type: TypeTaskClaimed})
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not start writing")
	}

	// buffer=1：第二条填满 buffer，第三条必然走 default 分支被丢弃。
	e.Emit(Event{Type: TypeTaskClaimed})
	e.Emit(Event{Type: TypeTaskClaimed})
	if got := e.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1（buffer 满后应恰好丢一条）", got)
	}

	sink.release() // 放行第一次 Write，Close 才能排空
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := len(strings.Split(strings.TrimSpace(sink.String()), "\n")); got != 2 {
		t.Fatalf("drained lines = %d, want 2", got)
	}
}

func TestEmitterEmitAfterCloseIsSafe(t *testing.T) {
	var w bufCloser
	e := NewEmitter("r-closed", NewJSONLSink(&w), 8, time.Now)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before := e.Dropped()
	e.Emit(Event{Type: TypeRunStopped}) // Close 之后调用：不得 panic
	if got := e.Dropped(); got != before+1 {
		t.Fatalf("dropped after close = %d, want %d", got, before+1)
	}
}

func TestNopEmitterIsSafe(t *testing.T) {
	var n Emitter = NopEmitter{}
	n.Emit(Event{Type: TypeRunStarted}) // 不 panic 即可
}

func TestEmitterCloseTimeoutCountsDropped(t *testing.T) {
	sink := newBlockingSink()
	e := NewEmitter("r-timeout", NewJSONLSink(sink), 4, time.Now)
	e.Emit(Event{Type: TypeTaskClaimed})
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not start writing")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消：drain 卡在写盘，Close 必然走超时分支
	err := e.Close(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("close err = %v, want context.Canceled", err)
	}
	if got := e.Dropped(); got == 0 {
		t.Fatal("close timeout should count into dropped")
	}

	sink.release() // 放行 drain，避免测试结束时还挂着写协程
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not finish after release")
	}
}

func TestEmitterConcurrentEmitAndClose(t *testing.T) {
	var w bufCloser
	e := NewEmitter("r-conc", NewJSONLSink(&w), 4, time.Now)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				e.Emit(Event{Type: TypeTaskClaimed})
			}
		}()
	}

	// 故意在还有 goroutine 在 Emit 时 Close：不得 panic、不得向已关闭 channel 发送。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	wg.Wait()

	before := e.Dropped()
	e.Emit(Event{Type: TypeRunStopped})
	if got := e.Dropped(); got != before+1 {
		t.Fatalf("dropped after close = %d, want %d", got, before+1)
	}
}
