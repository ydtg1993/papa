package app

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/engine"
)

func TestApplyStagePlanPropagatesAddStageError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := noDBEngine(t, cfg)
	t.Cleanup(func() { e.Stop(time.Second) })
	a := &App{Engine: e}
	err := a.applyStagePlan(stagePlan{stage: "bad", cfg: engine.StageConfig{WorkerCount: 0, QueueSize: 1}})
	if err == nil || !strings.Contains(err.Error(), "WorkerCount") {
		t.Fatalf("applyStagePlan must propagate AddStage validation error: %v", err)
	}
}
