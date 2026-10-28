package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func backlogInput() *obs.Input {
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	evs := []events.Event{
		{Type: events.TypeTaskFinished, TS: now.Add(-30 * time.Minute), Site: "a", Stage: "detail"},
		{Type: events.TypeTaskFinished, TS: now.Add(-20 * time.Minute), Site: "a", Stage: "detail"},
		{Type: events.TypeTaskFailed, TS: now.Add(-10 * time.Minute), Site: "a", Stage: "detail"},
	}
	return &obs.Input{
		Mode: obs.ModeAcceptance, Now: now,
		Run:    obs.RunMeta{From: now.Add(-time.Hour), To: now},
		Events: evs,
	}
}

func TestBacklogSectionAgingAndETA(t *testing.T) {
	q := &fakeQuerier{bucket: []TaskBucket{
		{Site: "a", Stage: "detail", Status: 0, Repeatable: 0, AgeBucket: "gt6h", N: 60},
		{Site: "a", Stage: "detail", Status: 1, Repeatable: 0, AgeBucket: "10m-1h", N: 6},
		{Site: "a", Stage: "detail", Status: 0, Repeatable: 1, AgeBucket: "lt10m", N: 900},
	}}
	res, err := (&BacklogSection{Querier: q}).Build(context.Background(), backlogInput())
	if err != nil {
		t.Fatal(err)
	}
	text := tableText(res)
	for _, want := range []string{"a", "detail", "gt6h", "lt10m"} {
		if !strings.Contains(text, want) {
			t.Fatalf("积压表缺 %q：\n%s", want, text)
		}
	}
	if len(res.Metrics.(obs.BacklogSummary).ETA) == 0 {
		t.Fatalf("应给出 ETA：%+v", res.Metrics)
	}
	if c := checkByID(res, "throughput_zero"); c != nil && !c.Passed {
		t.Fatalf("有吞吐时不该报 throughput_zero：%+v", c)
	}
}

func TestBacklogSectionPausedSkipsThroughputWarn(t *testing.T) {
	in := backlogInput()
	in.Mode = obs.ModeSnapshot
	in.Engine = obs.EngineState{Source: "engine", ActivePools: 0}
	q := &fakeQuerier{bucket: []TaskBucket{{Site: "a", Stage: "detail", Status: 0, N: 10}}}
	res, err := (&BacklogSection{Querier: q}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByID(res, "throughput_zero"); c == nil || !c.Skipped {
		t.Fatalf("暂停时 throughput_zero 应 skipped：%+v", c)
	}
	if !strings.Contains(tableText(res), "ETA 不适用") {
		t.Fatalf("暂停时 ETA 应标不适用：\n%s", tableText(res))
	}
}
