package rulequality

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type collectorRecorderFunc func(context.Context, Observation) (Observation, error)

func (f collectorRecorderFunc) Record(ctx context.Context, o Observation) (Observation, error) {
	return f(ctx, o)
}

func TestCollectorBoundedStableFailures(t *testing.T) {
	for _, panicRecorder := range []bool{false, true} {
		collector, err := NewCollector(collectorRecorderFunc(func(ctx context.Context, o Observation) (Observation, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 3*time.Second {
				t.Error("missing bounded deadline")
			}
			if panicRecorder {
				panic("secret-token")
			}
			return Observation{}, errors.New("secret-token")
		}), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		err = collector.Observe(context.Background(), metricWindow(1).Observations[0])
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unstable or missing error: %v", err)
		}
		if got := collector.Stats(); got.Attempted != 1 || got.Failed != 1 {
			t.Fatal(got)
		}
	}
}

func TestCollectorDetachesAndCountsConcurrentCalls(t *testing.T) {
	collector, err := NewCollector(collectorRecorderFunc(func(ctx context.Context, o Observation) (Observation, error) {
		o.Fields[0].Name = "changed"
		return o, nil
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	o := metricWindow(1).Observations[0]
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := collector.Observe(context.Background(), o); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if o.Fields[0].Name != "title" || collector.Stats().Attempted != 50 || collector.Stats().Failed != 0 {
		t.Fatalf("caller mutated or statistics lost: %+v", collector.Stats())
	}
}

func TestCollectorHonorsCancellationAndTimeout(t *testing.T) {
	calls := 0
	collector, err := NewCollector(collectorRecorderFunc(func(ctx context.Context, o Observation) (Observation, error) {
		calls++
		<-ctx.Done()
		return Observation{}, ctx.Err()
	}), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := collector.Observe(ctx, Observation{}); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("canceled context: calls=%d err=%v", calls, err)
	}
	if err := collector.Observe(context.Background(), metricWindow(1).Observations[0]); !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("deadline: calls=%d err=%v", calls, err)
	}
}

func TestCollectorRejectsUnboundedOrRawIdentityBeforeRecorder(t *testing.T) {
	calls := 0
	collector, err := NewCollector(collectorRecorderFunc(func(_ context.Context, o Observation) (Observation, error) { calls++; return o, nil }), 0)
	if err != nil {
		t.Fatal(err)
	}
	o := metricWindow(1).Observations[0]
	o.LogicalKey = "https://private/?token=secret"
	if err := collector.Observe(context.Background(), o); !errors.Is(err, ErrInvalid) {
		t.Fatalf("raw logical identity accepted: %v", err)
	}
	o.LogicalKey = ""
	o.Fields = make([]FieldStat, MaxFields+1)
	if err := collector.Observe(context.Background(), o); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded fields accepted: %v", err)
	}
	if calls != 0 || collector.Stats().Failed != 2 {
		t.Fatalf("calls=%d stats=%+v", calls, collector.Stats())
	}
}

func TestCollectorLogicalKeyNeverRetainsRawKey(t *testing.T) {
	ctx := WithLogicalKey(context.Background(), "https://secret.example/?token=private")
	key := LogicalKey(ctx)
	if len(key) != 64 || strings.Contains(key, "secret") || key != LogicalKey(WithLogicalKey(context.Background(), "https://secret.example/?token=private")) {
		t.Fatalf("logical key=%q", key)
	}
	if LogicalKey(context.Background()) != "" || LogicalKey(WithLogicalKey(ctx, "")) != "" {
		t.Fatal("empty host key must clear folding key")
	}
}
