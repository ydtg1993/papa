package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"gorm.io/gorm"
)

func TestRepeatScheduleCanceledBeforeQuery(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cancel()
	f.mu.Lock()
	before := len(f.queries)
	f.mu.Unlock()
	if _, ok := e.soonestRepeatIn(); ok {
		t.Fatal("canceled read returned a schedule")
	}
	f.mu.Lock()
	after := len(f.queries)
	f.mu.Unlock()
	if after != before {
		t.Fatal("canceled engine issued SQL")
	}
}

func TestRepeatScheduleUsesOneDatabaseRead(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.stages["stub:stub"].config.RepeatLimit = 3
	e.stages["peer:stub"] = &stageInfo{stage: "stub", config: StageConfig{Site: "peer", RepeatLimit: 3}}
	epoch := time.Now().Add(time.Hour).Unix()
	f.minEpoch = &epoch
	f.mu.Lock()
	before := len(f.queries)
	f.mu.Unlock()
	if d, ok := e.soonestRepeatIn(); !ok || d <= 0 {
		t.Fatalf("missing schedule: %s %v", d, ok)
	}
	f.mu.Lock()
	after := len(f.queries)
	f.mu.Unlock()
	if after-before != 1 {
		t.Fatalf("database reads=%d, want one", after-before)
	}
}

func TestRepeatScheduleStopWaitsForQueryExit(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.stages["stub:stub"].config.RepeatLimit = 3
	e.stages["peer:stub"] = &stageInfo{stage: "stub", config: StageConfig{Site: "peer", RepeatLimit: 3}}
	sql := e.db.ToSQL(func(d *gorm.DB) *gorm.DB { var row struct{ Epoch *int64 }; return e.soonestRepeatQueryOn(d).Find(&row) })
	if !strings.Contains(sql, "site IN (") {
		t.Fatal("shutdown fixture did not exercise grouped limits")
	}
	e.cfg.RepeatQueue.Enabled = true
	e.cfg.RepeatQueue.Interval = time.Hour
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := e.db.Callback().Row().Before("gorm:row").Register("test:hold_repeat_read", func(tx *gorm.DB) { once.Do(func() { close(entered); <-release; close(exited) }) }); err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	defer func() {
		e.cancel()
		releaseOnce.Do(func() { close(release) })
		e.tickerWG.Wait()
		e.Stop(time.Second)
		drainTestEvents(t, e)
	}()
	e.startRepeatQueue()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("query not entered")
	}
	if drained, _ := e.Stop(30 * time.Millisecond); drained {
		t.Fatal("Stop reported drained with query still in flight")
	}
	select {
	case <-exited:
		t.Fatal("query unexpectedly exited before release")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	e.tickerWG.Wait()
	select {
	case <-exited:
	default:
		t.Fatal("ticker left without query exit")
	}
	if drained, stats := e.Stop(time.Second); !drained || len(stats) != 0 {
		t.Fatalf("query did not drain: %v %v", drained, stats)
	}
}
