package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
)

func TestAddStageRejectsInvalidPoolConfig(t *testing.T) {
	for _, cfg := range []StageConfig{
		{WorkerCount: 0, QueueSize: 1}, {WorkerCount: -1, QueueSize: 1},
		{WorkerCount: 1, QueueSize: 0}, {WorkerCount: 1, QueueSize: -1},
	} {
		e := &Engine{stages: map[string]*stageInfo{}}
		err := e.AddStage("invalid", cfg, &recordingFetcher{stage: "invalid"}, nil)
		if err == nil || !strings.Contains(err.Error(), "must be > 0") {
			t.Errorf("AddStage(%+v) must return an explicit validation error", cfg)
		}
		if len(e.stages) != 0 {
			t.Errorf("invalid stage registered: %+v", cfg)
		}
	}
}

func TestBuildPoolRejectsInvalidConfigWithoutRegisteringNil(t *testing.T) {
	e := &Engine{
		cfg: &config.Config{}, loggerSet: &loggers.LoggerSet{Engine: quietLogger()}, pools: newPoolRegistry(),
		stages: map[string]*stageInfo{stubKey: {stage: "stub", config: StageConfig{Site: "stub", WorkerCount: 1, QueueSize: -1}}},
	}
	if p := e.getOrCreatePool(stubKey); p != nil {
		t.Fatal("invalid config built a pool")
	}
	if len(e.pools.pools) != 0 {
		t.Fatal("failed creation registered a nil pool")
	}
}

func TestStopSharesWorkerAndTickerDeadline(t *testing.T) {
	e, pool := urgentEngine(t, newFakeTaskDB())
	e.ctx, e.cancel = context.WithCancel(context.Background())
	release := make(chan struct{})
	started := make(chan struct{})
	workerDone := make(chan struct{})
	pool.Start(context.Background(), func(context.Context, *Task) error {
		close(started)
		<-release
		close(workerDone)
		return nil
	})
	e.tickerWG.Add(1)
	go func() { defer e.tickerWG.Done(); <-release }()
	t.Cleanup(func() { close(release); <-workerDone; e.tickerWG.Wait() })
	if err := pool.Submit(&Task{ID: 7, Stage: "stub", URL: "https://example.test"}); err != nil {
		t.Fatal(err)
	}
	<-started
	const budget = 200 * time.Millisecond
	start := time.Now()
	drained, stats := e.Stop(budget)
	elapsed := time.Since(start)
	if drained || stats[stubKey].Unfinished != 1 {
		t.Fatalf("blocked worker/ticker reported drained=%v stats=%+v", drained, stats)
	}
	if elapsed > budget+120*time.Millisecond {
		t.Fatalf("Stop used more than one budget: elapsed=%v budget=%v", elapsed, budget)
	}
}
