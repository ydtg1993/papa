package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func TestEventCoverageWarnsOnGap(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.Local)
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now,
		Run: obs.RunMeta{From: now.Add(-2 * time.Hour), To: now},
		Events: []events.Event{
			{Type: events.TypeRunStarted, TS: now.Add(-2 * time.Hour)},
			{Type: events.TypeTaskFinished, TS: now.Add(-time.Minute)},
		}}
	out, err := (&Generic{Thresholds: Thresholds{EventMaxGap: 10 * time.Minute}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(out, "event_coverage")
	if c == nil || c.Passed {
		t.Fatalf("event_coverage 应未通过：%+v", c)
	}
	if len(in.Degraded) == 0 {
		t.Fatal("时间断层应写入 degraded（fail-closed 用）")
	}
}

func TestEventCoverageSkippedWhenEngineInactive(t *testing.T) {
	now := time.Date(2026, 10, 29, 2, 0, 0, 0, time.Local)
	in := &obs.Input{Mode: obs.ModeSnapshot, Now: now,
		Engine: obs.EngineState{Source: "engine", ActivePools: 0},
		Events: []events.Event{{Type: events.TypeStageSnapshot, TS: now.Add(-time.Hour)}}}
	out, err := (&Generic{Thresholds: Thresholds{EventMaxGap: 10 * time.Minute}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByID(out, "event_coverage"); c == nil || !c.Skipped {
		t.Fatalf("暂停时 event_coverage 应 skipped：%+v", c)
	}
}
