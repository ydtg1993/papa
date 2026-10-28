package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func pausedInput(t *testing.T, mode obs.Mode) *obs.Input {
	t.Helper()
	fake := gormfake.New()
	fake.SetCounts(0, 804) // stuck / unfinished 都查 count(*)；具体值不重要
	now := time.Date(2026, 10, 29, 0, 27, 41, 0, time.Local)
	return &obs.Input{
		Mode: mode,
		Engine: obs.EngineState{
			Source: "engine", CapturedAt: now,
			Sites:       []obs.SiteState{{Key: "a", Paused: true}, {Key: "b", Paused: true}},
			ActivePools: 0,
		},
		Events: []events.Event{
			{Type: events.TypeStageSnapshot, TS: now.Add(-time.Minute)}, // 有快照事件但 stages 为空
		},
		DB:  gormfake.MustOpen(fake),
		Now: now,
	}
}

func checkByID(res *obs.SectionResult, id string) *obs.Check {
	for i := range res.Checks {
		if res.Checks[i].ID == id {
			return &res.Checks[i]
		}
	}
	return nil
}

func TestSnapshotPausedSkipsFrameworkLivenessChecks(t *testing.T) {
	res, err := (&Generic{}).Build(context.Background(), pausedInput(t, obs.ModeSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stuck_processing", "unfinished_tasks", "queue_drained"} {
		c := checkByID(res, id)
		if c == nil {
			t.Fatalf("缺少检查 %s", id)
		}
		if !c.Skipped || c.SkipReason == "" {
			t.Fatalf("%s 在暂停快照下应 skipped + reason，实得 %+v", id, c)
		}
	}
	for _, p := range res.Problems {
		if p.ID == "no-stage-snapshot" {
			t.Fatalf("暂停时不该再报 no-stage-snapshot：%+v", p)
		}
	}
}

func TestAcceptancePausedDoesNotSkip(t *testing.T) {
	res, err := (&Generic{}).Build(context.Background(), pausedInput(t, obs.ModeAcceptance))
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByID(res, "stuck_processing"); c == nil || c.Skipped {
		t.Fatalf("acceptance 下 stuck_processing 不应 skipped：%+v", c)
	}
	if c := checkByID(res, "unfinished_tasks"); c == nil || c.Skipped {
		t.Fatalf("acceptance 下 unfinished_tasks 不应 skipped：%+v", c)
	}
}
