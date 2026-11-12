package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/models"
)

type shutdownCheckpointHook struct{ finish func() }

func (h shutdownCheckpointHook) Levels() []logrus.Level { return []logrus.Level{logrus.WarnLevel} }
func (h shutdownCheckpointHook) Fire(entry *logrus.Entry) error {
	if entry.Message == "dynamic tickers still running after timeout" {
		h.finish()
	}
	return nil
}

func TestEngineStopSnapshotsAfterTickerFinalization(t *testing.T) {
	for _, settle := range []bool{false, true} {
		t.Run(map[bool]string{false: "late-retention", true: "late-confirmation"}[settle], func(t *testing.T) {
			e, pool := urgentEngine(t, newFakeTaskDB())
			e.ctx, e.cancel = context.WithCancel(context.Background())
			pool.Start(context.Background(), func(context.Context, *Task) error { return nil })
			if settle {
				e.execution.Remember(42, "stub:stub")
			}
			release, finished := make(chan struct{}), make(chan struct{})
			e.tickerWG.Add(1)
			go func() {
				<-release
				if settle {
					e.execution.Forget(42)
				} else {
					e.overflow.Put(&Task{ID: 41, Site: "stub", Stage: "stub"})
					e.execution.Remember(42, "stub:stub")
				}
				e.tickerWG.Done()
				close(finished)
			}()
			// The timeout log is a deterministic checkpoint inside Stop: finish a
			// maintenance operation after waiting starts, before its final report.
			e.loggerSet.Engine.AddHook(shutdownCheckpointHook{finish: func() { close(release); <-finished }})
			_, stats := e.Stop(20 * time.Millisecond)
			if settle {
				if len(stats) != 0 {
					t.Fatalf("reported persistence that ticker confirmed: %+v", stats)
				}
			} else if got := stats["stub:stub"]; got.Queued != 1 || got.Unfinished != 1 {
				t.Fatalf("lost ticker's final retained work: %+v", stats)
			}
		})
	}
}

func TestEngineStopReportsDelayedTasksByStage(t *testing.T) {
	e, pool := urgentEngine(t, newFakeTaskDB())
	e.ctx, e.cancel = context.WithCancel(context.Background())
	pool.Start(context.Background(), func(context.Context, *Task) error { return nil })
	for i, stage := range []string{"detail", "detail", "catalog"} {
		task := &Task{ID: i + 1, Site: "fixture", Stage: stage}
		e.enqueueDelayed(time.Now().Add(time.Hour), task, models.CrawlerTask{ID: uint(task.ID)})
	}
	drained, stats := e.Stop(time.Second)
	if drained || stats["fixture:detail"].Queued != 2 || stats["fixture:catalog"].Queued != 1 {
		t.Fatalf("delayed tasks missing from shutdown: drained=%v stats=%+v", drained, stats)
	}
	if e.delays.Len() != 3 {
		t.Fatal("shutdown discarded delayed tasks")
	}
}
