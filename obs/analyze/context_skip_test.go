package analyze

import (
	"context"
	"strings"
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
	if c := checkByID(res, "stuck_processing"); c == nil || c.Skipped || c.Severity != obs.SeverityFail {
		t.Fatalf("acceptance 下 stuck_processing 不应 skipped 且仍是 fail：%+v", c)
	}
	if c := checkByID(res, "unfinished_tasks"); c == nil || c.Skipped || c.Severity != obs.SeverityFail {
		t.Fatalf("acceptance 下 unfinished_tasks 不应 skipped 且仍是 fail：%+v", c)
	}
}

// snapshot 是「现在怎样」：引擎在跑时 pending/processing 非 0 是常态，不能按验收口径判 fail，
// 应降为 warn（acceptance 仍 fail；暂停快照仍 skipped，见上两个测试）。
func TestSnapshotActiveDowngradesLivenessChecksToWarn(t *testing.T) {
	active := pausedInput(t, obs.ModeSnapshot)
	active.Engine = obs.EngineState{
		Source: "engine", CapturedAt: active.Now,
		Sites:       []obs.SiteState{{Key: "a"}},
		ActivePools: 2,
	}
	res, err := (&Generic{}).Build(context.Background(), active)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stuck_processing", "unfinished_tasks"} {
		c := checkByID(res, id)
		if c == nil || c.Skipped {
			t.Fatalf("%s 在活跃快照下不应 skipped：%+v", id, c)
		}
		if c.Severity != obs.SeverityWarn {
			t.Fatalf("%s 在活跃快照下应降为 warn，实得 %q：%+v", id, c.Severity, c)
		}
		if !strings.Contains(c.Detail, "snapshot 快照不按验收口径判 fail") {
			t.Fatalf("%s 的 detail 应注明 snapshot 口径：%q", id, c.Detail)
		}
	}
}
