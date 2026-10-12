package engine

import (
	"fmt"
	"testing"

	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
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
		stages:      map[string]*stageInfo{"stub": {config: StageConfig{Site: "a"}}},
		stageTotals: map[string]stageTotals{"stub": {Submitted: 10, Completed: 8, Failed: 2}},
	}
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	_ = pool.Submit(&Task{Stage: "stub", URL: "https://example.com"}) // 当前池 submitted=1
	e.setStatsQueue("stub", track.NewStatsQueue[*Task](pool))

	got := e.GetStageStats()["stub"]
	if got.Queue.Submitted != 11 || got.Queue.Completed != 8 || got.Queue.Failed != 2 {
		t.Fatalf("累计+当前池应相加：submitted=%d completed=%d failed=%d",
			got.Queue.Submitted, got.Queue.Completed, got.Queue.Failed)
	}
	if got.Site != "a" {
		t.Fatalf("Site 应照旧取站点，实得 %q", got.Site)
	}
}
