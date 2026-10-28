package pattern

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func TestBuiltinPatternsMatchExpectedEvidence(t *testing.T) {
	now := time.Now()
	in := &obs.Input{
		EventMeta: obs.EventMeta{Gaps: 3},
		Events: []events.Event{
			{Type: events.TypeRunStarted, TS: now},
			{Type: events.TypeRunStarted, TS: now.Add(time.Minute)},
			{Type: events.TypeBreakerTripped, Site: "s1"},
			{Type: events.TypeBreakerTripped, Site: "s1"},
			{Type: events.TypeStageSnapshot, Stages: map[string]events.StageSnapshot{
				"s:c": {Site: "s", Stage: "c", QueueLen: 2},
			}},
		},
		Logs: obs.LogSummary{Signatures: []obs.LogSignature{{
			Pattern: "recover queue: stage <id> not registered", Count: 4,
			Samples: []obs.LogSample{{File: "engine.log", Line: 7, Msg: "recover queue: stage s:c not registered"}},
		}}},
	}
	got := map[string]bool{}
	for _, p := range Builtin() {
		for _, prob := range p.Match(in) {
			got[prob.ID] = true
			if prob.Title == "" || prob.Severity == "" {
				t.Errorf("%s 产出不完整：%+v", p.Key(), prob)
			}
		}
	}
	for _, id := range []string{"event-loss", "process-restarts", "queue-not-draining", "breaker-noise", "stage-not-registered"} {
		if !got[id] {
			t.Errorf("缺少问题 %s", id)
		}
	}
}

func TestBuiltinNoEvidenceNoProblems(t *testing.T) {
	in := &obs.Input{}
	for _, p := range Builtin() {
		if probs := p.Match(in); len(probs) != 0 {
			t.Errorf("%s 无证据时不应产出问题：%+v", p.Key(), probs)
		}
	}
}
