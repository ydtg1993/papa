package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/track"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
)

// 收割把被回收池的最终吞吐累进该阶段的累计值，多次收割累加。
func TestHarvestStageTotalsAccumulates(t *testing.T) {
	e := &Engine{stageTotals: make(map[string]stageTotals)}

	pool1 := workerpool.NewWorkerPool[*Task](1, 8, 1)
	_ = pool1.Submit(&Task{Stage: "s", URL: "a"})
	_ = pool1.Submit(&Task{Stage: "s", URL: "b"})
	e.harvestStageTotals("s", pool1)
	if got := e.stageTotals["s"].Submitted; got != 2 {
		t.Fatalf("应收割 submitted=2，实得 %d", got)
	}

	pool2 := workerpool.NewWorkerPool[*Task](1, 8, 1)
	for i := 0; i < 3; i++ {
		_ = pool2.Submit(&Task{Stage: "s", URL: fmt.Sprintf("c%d", i)})
	}
	e.harvestStageTotals("s", pool2)
	if got := e.stageTotals["s"].Submitted; got != 5 {
		t.Fatalf("应累计 submitted=5，实得 %d", got)
	}
}

// GetStageStats 汇报「累计（跨池实例）+ 当前池」，池 churn 后吞吐不归零。
func TestGetStageStatsAddsCumulative(t *testing.T) {
	e := &Engine{
		stages:      map[string]*stageInfo{"a:stub": {stage: "stub", config: StageConfig{Site: "a"}}},
		stageTotals: map[string]stageTotals{"a:stub": {Submitted: 10, Completed: 8, Failed: 2}},
	}
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	_ = pool.Submit(&Task{Stage: "stub", URL: "https://example.com"}) // 当前池 submitted=1
	e.setStatsQueue("a:stub", track.NewStatsQueue[*Task](pool))

	got := e.GetStageStats()["a:stub"]
	if got.Queue.Submitted != 11 || got.Queue.Completed != 8 || got.Queue.Failed != 2 {
		t.Fatalf("累计+当前池应相加：submitted=%d completed=%d failed=%d",
			got.Queue.Submitted, got.Queue.Completed, got.Queue.Failed)
	}
	if got.Site != "a" {
		t.Fatalf("Site 应照旧取站点，实得 %q", got.Site)
	}
}

func completedStatsPool(t *testing.T) *workerpool.WorkerPool[*Task] {
	t.Helper()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	pool.Start(context.Background(), func(_ context.Context, task *Task) error {
		if task.URL == "failed" {
			return errors.New("synthetic failure")
		}
		return nil
	})
	t.Cleanup(func() { pool.Stop(time.Second) })
	for _, url := range []string{"completed", "failed"} {
		if err := pool.Submit(&Task{Stage: "stub", URL: url}); err != nil {
			t.Fatal(err)
		}
	}
	if drained, inFlight := pool.Stop(time.Second); !drained || inFlight != 0 {
		t.Fatalf("synthetic pool did not finish: drained=%v inFlight=%d", drained, inFlight)
	}
	return pool
}

// Pausing immediately after harvest recreates a legal monitor-read interleaving
// in the old reaper's two separate critical sections: totals already included
// the old pool, but its live snapshot remained visible and was added twice.
func TestHarvestStageTotalsRetiresCurrentSnapshotAtomically(t *testing.T) {
	const key = "a:stub"
	e := &Engine{}
	pool := completedStatsPool(t)
	e.setStatsQueue(key, track.NewStatsQueue(pool))
	before := e.GetStageStats()[key].Queue
	if before.Submitted != 2 || before.Completed != 1 || before.Failed != 1 {
		t.Fatalf("unexpected completed-pool baseline: %+v", before)
	}
	e.harvestStageTotals(key, pool)
	if stage, present := e.GetStageStats()[key]; present {
		t.Fatalf("retired pool remained observable after harvest: before=%+v after=%+v (old totals must not be counted twice)", before, stage.Queue)
	}
	if got := e.stageTotals[key]; got != (stageTotals{Submitted: 2, Completed: 1, Failed: 1}) {
		t.Fatalf("retiring the pool lost its cumulative totals: %+v", got)
	}
	newPool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	if err := newPool.Submit(&Task{Stage: "stub", URL: "queued"}); err != nil {
		t.Fatal(err)
	}
	stats := track.NewStatsQueue(newPool)
	t.Cleanup(stats.Stop)
	e.setStatsQueue(key, stats)
	got := e.GetStageStats()[key].Queue
	if got.Submitted != 3 || got.Completed != 1 || got.Failed != 1 || got.InProgress != 1 || got.QueueLen != 1 {
		t.Fatalf("replacement pool must add its own work to retired totals exactly once: %+v", got)
	}
}

func TestHarvestStageTotalsPreservesReplacementSnapshot(t *testing.T) {
	const key = "a:stub"
	e := &Engine{}
	oldPool := completedStatsPool(t)
	e.setStatsQueue(key, track.NewStatsQueue(oldPool))
	newPool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	if err := newPool.Submit(&Task{Stage: "stub", URL: "queued"}); err != nil {
		t.Fatal(err)
	}
	newStats := track.NewStatsQueue(newPool)
	t.Cleanup(newStats.Stop)
	e.setStatsQueue(key, newStats)
	// Lazy recreation can happen after the registry releases the retired pool
	// and before the reaper publishes its final counters.
	e.harvestStageTotals(key, oldPool)
	if e.statsQueue[key] != newStats {
		t.Fatal("harvesting the retired pool removed its replacement's statistics")
	}
	got := e.GetStageStats()[key].Queue
	if got.Submitted != 3 || got.Completed != 1 || got.Failed != 1 || got.InProgress != 1 || got.QueueLen != 1 {
		t.Fatalf("replacement snapshot lost old totals or counted them twice: %+v", got)
	}
}
