package engine

import (
	"context"

	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
)

func TestEvidenceFacadeBareEngineAndFreshConfig(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	if e.newTrace(nil, 0) != nil {
		t.Fatal("disabled recording must be a no-op")
	}
	e.cfg.Crawler.Trace.Mode = config.TraceOff
	if _, err := e.ListTraceExecution(7); err == nil {
		t.Fatal("disabled list should report configuration error")
	}
	if e.traceRetention() != defaultTraceRetention {
		t.Fatal("zero defaults changed")
	}
	e.cfg.Crawler.Trace.Retention = -1
	if e.traceRetention() != 0 {
		t.Fatal("facade cached stale configuration")
	}
}

func TestEvidenceMaintenanceEngineStopDrainsCleanup(t *testing.T) {
	e, pool := urgentEngine(t, newFakeTaskDB())
	e.ctx, e.cancel = context.WithCancel(context.Background())
	pool.Start(context.Background(), func(context.Context, *Task) error { return nil })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	startEvidenceMaintenance(e.ctx, &e.tickerWG, time.Millisecond, func(ctx context.Context) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start")
	}
	if drained, _ := e.Stop(20 * time.Millisecond); drained {
		t.Fatal("Stop returned drained while cleanup was blocked")
	}
	unblock()
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("Stop did not drain completed cleanup")
	}
}
