package pattern

import (
	"strings"
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
			{Type: events.TypeTaskClaimed, Site: "s", Stage: "c"},
			{Type: events.TypeStageSnapshot, Stages: map[string]events.StageSnapshot{
				"s:c": {Site: "s", Stage: "c", QueueLen: 2, InProgress: 2},
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

func TestProcessRestartRequiresUncertainOrAbnormalStop(t *testing.T) {
	now := time.Now()
	start := events.Event{Type: events.TypeRunStarted, RunID: "run-a", TS: now}
	next := events.Event{Type: events.TypeRunStarted, RunID: "run-b", TS: now.Add(2 * time.Minute)}
	for _, tc := range []struct {
		name string
		stop *events.Event
		want int
	}{
		{"planned", &events.Event{RunID: "run-a", Outcome: "drained"}, 0},
		{"missing", nil, 1},
		{"unknown", &events.Event{RunID: "run-a", Outcome: "unknown"}, 1},
		{"partial-with-recoverable-work", &events.Event{RunID: "run-a", Outcome: "partial", Counts: map[string]int{"unfinished": 2, "queued": 1}}, 0},
		{"stop-for-other-run", &events.Event{RunID: "different", Outcome: "drained"}, 1},
		{"drained-with-error", &events.Event{RunID: "run-a", Outcome: "drained", Error: "shutdown error"}, 1},
		{"drained-with-backlog", &events.Event{RunID: "run-a", Outcome: "drained", Counts: map[string]int{"queued": 2}}, 0},
		{"partial-with-error", &events.Event{RunID: "run-a", Outcome: "partial", Error: "shutdown error"}, 1},
		{"abnormal-outcome", &events.Event{RunID: "run-a", Outcome: "crashed"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := []events.Event{start}
			if tc.stop != nil {
				stop := *tc.stop
				stop.Type = events.TypeRunStopped
				stop.TS = now.Add(time.Minute)
				evs = append(evs, stop)
			}
			evs = append(evs, next)
			probs := (&ProcessRestartPattern{}).Match(&obs.Input{Events: evs})
			if len(probs) != tc.want {
				t.Fatalf("problems=%+v want count=%d", probs, tc.want)
			}
			if len(probs) > 0 {
				if len(probs[0].Evidence) == 0 || !strings.Contains(probs[0].Hypotheses[0].Text, "不足以确定") {
					t.Fatalf("未知退出应提供证据而不推断异常原因: %+v", probs[0])
				}
			}
		})
	}
}

func TestProcessRestartPairsStopsByRunIDAndTime(t *testing.T) {
	now := time.Now()
	probs := (&ProcessRestartPattern{}).Match(&obs.Input{Events: []events.Event{
		{Type: events.TypeRunStarted, RunID: "run-c", TS: now.Add(4 * time.Minute)},
		{Type: events.TypeRunStopped, RunID: "run-a", Outcome: "drained", TS: now.Add(time.Minute)},
		{Type: events.TypeRunStarted, RunID: "run-a", TS: now},
		{Type: events.TypeRunStarted, RunID: "run-b", TS: now.Add(2 * time.Minute)},
		{Type: events.TypeRunStopped, RunID: "run-b", Outcome: "drained", TS: now.Add(3 * time.Minute)},
	}})
	if len(probs) != 0 {
		t.Fatalf("多次计划重启不应告警: %+v", probs)
	}
}

func TestProcessRestartIgnoresDuplicateStartForSameRun(t *testing.T) {
	now := time.Now()
	probs := (&ProcessRestartPattern{}).Match(&obs.Input{Events: []events.Event{
		{Type: events.TypeRunStarted, RunID: "run-a", TS: now},
		{Type: events.TypeRunStarted, RunID: "run-a", TS: now.Add(time.Second)},
	}})
	if len(probs) != 0 {
		t.Fatalf("同一 run 重复开始不能算重启: %+v", probs)
	}
}

func TestQueueNotDrainingSkipsPausedAndInactiveWindows(t *testing.T) {
	now := time.Now()
	snapshot := events.Event{Type: events.TypeStageSnapshot, TS: now, Stages: map[string]events.StageSnapshot{
		"s:c": {Site: "s", QueueLen: 10, InProgress: 12},
	}}
	for _, tc := range []struct {
		name   string
		engine obs.EngineState
		active bool
	}{
		{"no-execution", obs.EngineState{Source: "engine", ActivePools: 1}, false},
		{"manual-pause", obs.EngineState{Source: "engine", ActivePools: 1, Sites: []obs.SiteState{{Key: "s", Paused: true, PauseSource: "manual"}}}, true},
		{"startup-pause", obs.EngineState{Source: "engine", Sites: []obs.SiteState{{Key: "s", Paused: true, PauseSource: "manual", PauseReason: "StartPaused"}}}, true},
		{"no-pools", obs.EngineState{Source: "engine"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &obs.Input{Mode: obs.ModeSnapshot, Engine: tc.engine, Events: []events.Event{snapshot}}
			if tc.active {
				in.Events = append(in.Events, events.Event{Type: events.TypeTaskClaimed, Site: "s"})
			}
			if probs := (&QueueNotDrainingPattern{}).Match(in); len(probs) != 0 {
				t.Fatalf("非执行/暂停窗口不应报积压故障: %+v", probs)
			}
		})
	}
}

func TestQueueNotDrainingCountsOutstandingOnce(t *testing.T) {
	in := &obs.Input{Mode: obs.ModeSnapshot, Engine: obs.EngineState{Source: "engine", ActivePools: 1}, Events: []events.Event{
		{Type: events.TypeTaskClaimed, Site: "s", Stage: "c"},
		{Type: events.TypeStageSnapshot, Stages: map[string]events.StageSnapshot{
			"s:c": {Site: "s", QueueLen: 10, InProgress: 12},
		}},
	}}
	probs := (&QueueNotDrainingPattern{}).Match(in)
	if len(probs) != 1 || probs[0].Count != 12 {
		t.Fatalf("InProgress 已含排队，只应计 12: %+v", probs)
	}
}

func TestQueueNotDrainingSkipsPausedSitesInMixedWindow(t *testing.T) {
	in := &obs.Input{Mode: obs.ModeSnapshot, Engine: obs.EngineState{Source: "engine", ActivePools: 1, Sites: []obs.SiteState{
		{Key: "paused", Paused: true, PauseSource: "manual"}, {Key: "active"},
	}}, Events: []events.Event{
		{Type: events.TypeTaskClaimed, Site: "paused"},
		{Type: events.TypeTaskClaimed, Site: "active"},
		{Type: events.TypeStageSnapshot, Stages: map[string]events.StageSnapshot{
			"paused:c": {Site: "paused", QueueLen: 20, InProgress: 25},
			"active:c": {Site: "active", QueueLen: 3, InProgress: 4},
		}},
	}}
	probs := (&QueueNotDrainingPattern{}).Match(in)
	if len(probs) != 1 || probs[0].Count != 4 {
		t.Fatalf("不应把暂停站点算入异常积压: %+v", probs)
	}
}

func TestQueueNotDrainingRespectsLatestEmptySnapshot(t *testing.T) {
	now := time.Now()
	in := &obs.Input{Events: []events.Event{
		{Type: events.TypeTaskClaimed, Site: "s"},
		{Type: events.TypeStageSnapshot, TS: now, Stages: map[string]events.StageSnapshot{"s:c": {Site: "s", InProgress: 4}}},
		{Type: events.TypeStageSnapshot, TS: now.Add(time.Minute)},
	}}
	if probs := (&QueueNotDrainingPattern{}).Match(in); len(probs) != 0 {
		t.Fatalf("末次空池快照应覆盖旧积压: %+v", probs)
	}
}

func TestPausedEngineKeepsIndependentErrorEvidence(t *testing.T) {
	in := &obs.Input{
		Mode: obs.ModeSnapshot,
		Engine: obs.EngineState{Source: "engine", Sites: []obs.SiteState{
			{Key: "s", Paused: true, PauseSource: "manual"},
		}},
		EventMeta: obs.EventMeta{Gaps: 1},
		Events: []events.Event{
			{Type: events.TypeBreakerTripped, Site: "s"},
			{Type: events.TypeBreakerTripped, Site: "s"},
		},
		Logs: obs.LogSummary{Signatures: []obs.LogSignature{{Pattern: "stage not registered", Count: 1}}},
	}
	got := map[string]bool{}
	for _, p := range Builtin() {
		for _, prob := range p.Match(in) {
			got[prob.ID] = true
		}
	}
	for _, id := range []string{"event-loss", "breaker-noise", "stage-not-registered"} {
		if !got[id] {
			t.Fatalf("暂停不应隐藏独立错误证据 %s: %v", id, got)
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
