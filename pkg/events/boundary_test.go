package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type eventFunc func(Event)

func (f eventFunc) Emit(e Event) { f(e) }

type closingSink struct {
	entered, release       chan struct{}
	calls                  atomic.Int32
	panicWrite, panicClose bool
}

func (s *closingSink) Write(p []byte) (int, error) {
	if s.panicWrite {
		panic("write")
	}
	return len(p), nil
}
func (s *closingSink) Close() error {
	s.calls.Add(1)
	if s.panicClose {
		panic("close")
	}
	close(s.entered)
	<-s.release
	return nil
}

func TestDetachBoundsAndOwnsAllMaps(t *testing.T) {
	e := Event{URL: strings.Repeat("x", MaxStringBytes+1), Counts: map[string]int{"a": 1}, Attrs: map[string]string{"b": "before"}, Stages: map[string]StageSnapshot{"c": {Site: "before"}}, Queues: map[string]QueueSnapshot{"d": {LastError: "before"}}}
	got, complete := Detach(e)
	e.Counts["a"] = 9
	e.Attrs["b"] = "after"
	e.Stages["c"] = StageSnapshot{}
	e.Queues["d"] = QueueSnapshot{}
	if complete || !got.Truncated || got.URL != "[omitted: event string budget]" || got.Counts["a"] != 1 || got.Attrs["b"] != "before" || got.Stages["c"].Site != "before" || got.Queues["d"].LastError != "before" {
		t.Fatalf("bad detached value: %+v", got)
	}
	many := map[string]string{}
	for i := 0; i < 1000; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	got, complete = Detach(Event{Attrs: many})
	if complete || len(got.Attrs) > MaxMapEntries {
		t.Fatal("map budget not enforced")
	}
}

func TestIsolateBlockedConsumerOwnsPayloadAndDrops(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	received := make(chan Event, 1)
	e := Isolate(eventFunc(func(ev Event) {
		select {
		case <-entered:
			received <- ev
		default:
			close(entered)
			<-release
		}
	}))
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	e.Emit(Event{})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("consumer did not enter")
	}
	attrs := map[string]string{"a": "before"}
	e.Emit(Event{Attrs: attrs})
	attrs["a"] = "after"
	// Release only after proving Close does not wait for the user callback.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 20; i++ {
		if !errors.Is(e.Close(ctx), context.Canceled) {
			t.Fatal("close must honor cancellation")
		}
	}
	releaseOnce.Do(func() { close(release) })
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := e.Close(ctx2); err != nil {
		t.Fatal(err)
	}
	if (<-received).Attrs["a"] != "before" {
		t.Fatal("input alias retained")
	}
}

func TestEmitterCloseBudgetIncludesSinkClose(t *testing.T) {
	s := &closingSink{entered: make(chan struct{}), release: make(chan struct{})}
	e := NewEmitter("run", NewJSONLSink(s), 1, nil)
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(s.release) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if !errors.Is(e.Close(ctx), context.DeadlineExceeded) {
		t.Fatal("close blocked outside budget")
	}
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		t.Fatal("sink close did not enter")
	}
	for i := 0; i < 20; i++ {
		_ = e.Close(ctx)
	}
	if s.calls.Load() != 1 {
		t.Fatal("duplicate close workers")
	}
	releaseOnce.Do(func() { close(s.release) })
	joined, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := e.Close(joined); err != nil {
		t.Fatal(err)
	}
}

func TestEmitterContainsWriteAndClosePanic(t *testing.T) {
	for _, write := range []bool{true, false} {
		s := &closingSink{panicWrite: write, panicClose: true}
		e := NewEmitter("run", NewJSONLSink(s), 1, nil)
		e.Emit(Event{})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := e.Close(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("panic must be contained and joined, got %v", err)
		}
		cancel()
		if e.Dropped() == 0 {
			t.Fatal("panic not counted")
		}
	}
}

func TestIsolateFloodKeepsOneBlockedConsumer(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	e := Isolate(eventFunc(func(Event) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}))
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := e.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	e.Emit(Event{})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("consumer did not enter")
	}
	for i := 0; i < 10000; i++ {
		e.Emit(Event{})
	}
	if calls.Load() != 1 || e.Dropped() != 10000-defaultBuffer {
		t.Fatalf("calls=%d drops=%d", calls.Load(), e.Dropped())
	}
	releaseOnce.Do(func() { close(release) })
	joined, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := e.Close(joined); err != nil {
		t.Fatal(err)
	}
}

func TestIsolatePanicDisablesConsumer(t *testing.T) {
	var calls atomic.Int32
	e := Isolate(eventFunc(func(Event) { calls.Add(1); panic("consumer") }))
	e.Emit(Event{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected contained and joined panic, got %v", err)
	}
	e.Emit(Event{})
	if calls.Load() != 1 || e.Dropped() < 2 {
		t.Fatal("panicked consumer remains active")
	}
}

func TestEmitterPreservesAdmissionTimeAcrossBlockedArchive(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		t.Run(map[bool]string{false: "default_clock", true: "outer_isolation"}[isolated], func(t *testing.T) {
			sink := newBlockingSink()
			var release sync.Once
			var clock func() time.Time
			if isolated {
				clock = time.Now
			}
			archive := NewEmitter("run", NewJSONLSink(sink), 4, clock)
			var ingress Closer = archive
			if isolated {
				// A forwarding emitter forces the same two queue boundaries as App.
				ingress = Isolate(eventFunc(archive.Emit))
			}
			t.Cleanup(func() {
				release.Do(sink.release)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := ingress.Close(ctx); err != nil {
					t.Error(err)
				}
				if err := archive.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			ingress.Emit(Event{Type: TypeRunStarted})
			select {
			case <-sink.entered:
			case <-time.After(time.Second):
				t.Fatal("archive did not block")
			}
			before := time.Now()
			ingress.Emit(Event{Type: TypeTaskFinished})
			after := time.Now()
			// Close the forwarding boundary first to ensure both events reached
			// the blocked archive without relying on scheduler timing.
			if isolated {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := ingress.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			// Ensure the OS wall clock can distinguish admission from release.
			<-time.After(5 * time.Millisecond)
			release.Do(sink.release)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := archive.Close(ctx); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(sink.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("got %d events", len(lines))
			}
			var ev Event
			if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
				t.Fatal(err)
			}
			if ev.TS.Before(before) || ev.TS.After(after) {
				t.Fatalf("event timestamp %s outside admission interval [%s, %s]", ev.TS, before, after)
			}
		})
	}
}

func TestEmitterConcurrentAdmissionKeepsSequenceOrder(t *testing.T) {
	var seen []uint64
	e := newBuffered(maxBuffer, func(ev Event) error { seen = append(seen, ev.Seq); return nil }, func() error { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() {
		if err := e.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 256; j++ {
				e.Emit(Event{})
			}
		}()
	}
	close(start)
	wg.Wait()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 4096 || e.Dropped() != 0 {
		t.Fatalf("received=%d dropped=%d", len(seen), e.Dropped())
	}
	for i, seq := range seen {
		if seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d", i, seq)
		}
	}
}
