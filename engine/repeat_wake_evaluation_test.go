package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestAlgorithmRepeatWakeLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		e := &Engine{ctx: ctx, cancel: cancel, wakeCoalesce: defaultWakeCoalesce}
		defer func() { cancel(); e.tickerWG.Wait() }()
		wake := make(chan struct{}, 1)
		var queries, ticks atomic.Int64
		e.runDynamicTicker(func() time.Duration { queries.Add(1); return repeatMinTick }, wake, func() { ticks.Add(1) })
		synctest.Wait()
		for i := 0; i < 30000; i++ {
			select {
			case wake <- struct{}{}:
			default:
			}
			time.Sleep(time.Millisecond)
		}
		synctest.Wait()
		// Do not count a coalescing window which was still open at 30s.
		q, n := queries.Load(), ticks.Load()
		cancel()
		e.tickerWG.Wait()
		if q < 100 || q > 125 || n != 3 {
			t.Fatalf("queries=%d ticks=%d", q, n)
		}
		t.Logf("signals=30000 virtual_seconds=30 interval_recomputations=%d ticks=%d", q, n)
	})
}
