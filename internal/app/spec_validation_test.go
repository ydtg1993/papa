package app

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/sitedecl"
)

func TestApplyStagePlanPropagatesAddStageError(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := noDBEngine(t, cfg)
	t.Cleanup(func() { e.Stop(time.Second) })
	a := &App{Engine: e}
	err := a.applyStagePlan(sitedecl.StagePlan{Stage: "bad", Config: engine.StageConfig{WorkerCount: 0, QueueSize: 1}})
	if err == nil || !strings.Contains(err.Error(), "WorkerCount") {
		t.Fatalf("applyStagePlan must propagate AddStage validation error: %v", err)
	}
}
