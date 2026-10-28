package analyze

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func stageSnapshot(ts time.Time, queueLen int) events.Event {
	return events.Event{
		Type: events.TypeStageSnapshot, TS: ts,
		Stages: map[string]events.StageSnapshot{
			"s:catalog": {Site: "s", Stage: "catalog", QueueLen: queueLen, InProgress: 0, Workers: 1, Completed: 8, Failed: 1, Submitted: 9},
		},
		Queues: map[string]events.QueueSnapshot{
			"error_queue": {Enabled: true, Backlog: 0},
		},
	}
}

func TestGenericBuildsChecksTablesProblems(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	evs := []events.Event{
		{Type: events.TypeRunStarted, TS: now.Add(-time.Hour), RunID: "r-1"},
		stageSnapshot(now.Add(-30*time.Minute), 2),
		stageSnapshot(now.Add(-time.Minute), 0),
		{Type: events.TypeTaskFinished, TS: now.Add(-20 * time.Minute), Site: "s", Stage: "catalog", DurationMS: 100},
		{Type: events.TypeTaskFailed, TS: now.Add(-10 * time.Minute), Site: "s", Stage: "catalog", Kind: "structure",
			TaskID: 7, URL: "https://example.test/a", Error: "selector miss"},
		{Type: events.TypeRunStopped, TS: now.Add(-time.Minute), RunID: "r-1"},
	}
	in := &obs.Input{
		Run:       obs.RunMeta{RunIDs: []string{"r-1"}, From: now.Add(-time.Hour), To: now},
		EventMeta: obs.EventMeta{Count: len(evs), Files: 1},
		Events:    evs,
		Logs: obs.LogSummary{
			Files: 1,
			Signatures: []obs.LogSignature{{
				Pattern: "task failed: #", Count: 1,
				Samples: []obs.LogSample{{File: "engine.log", Line: 12, Level: "error", Msg: "task failed: selector miss"}},
			}},
		},
		Now: now,
	}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}

	checks := map[string]obs.Check{}
	for _, c := range res.Checks {
		checks[c.ID] = c
	}
	// 1 成功 1 失败 → 失败率 50% > 5%，WARN 未通过。
	if c, ok := checks["crawl_fail_rate"]; !ok || c.Passed {
		t.Fatalf("crawl_fail_rate 应未通过：%+v", c)
	}
	// 最后一次快照积压 0 → 通过。
	if c, ok := checks["queue_drained"]; !ok || !c.Passed {
		t.Fatalf("queue_drained 应通过：%+v", c)
	}
	// 无 DB → 降级且不产生 stuck 检查。
	if c, ok := checks["stuck_processing"]; ok {
		t.Fatalf("无 DB 时不应有 stuck 检查：%+v", c)
	}
	if len(in.Degraded) == 0 {
		t.Fatal("无 DB 应记录 degraded")
	}

	// 失败分组问题带证据与假设。
	var found *obs.Problem
	for i := range res.Problems {
		if strings.Contains(res.Problems[i].ID, "task-failed") {
			found = &res.Problems[i]
		}
	}
	if found == nil {
		t.Fatalf("应有 task-failed 问题：%+v", res.Problems)
	}
	if found.Count != 1 || len(found.Hypotheses) == 0 || len(found.Evidence) == 0 {
		t.Fatalf("问题内容不完整：%+v", found)
	}
	if !strings.Contains(found.Evidence[len(found.Evidence)-1].Ref, "engine.log:12") {
		t.Fatalf("应引用日志样本：%+v", found.Evidence)
	}

	// 表格：运行边界 / 队列快照 / 失败分组 / 耗时。
	if len(res.Tables) < 4 {
		t.Fatalf("tables = %d, want >= 4", len(res.Tables))
	}
}

func TestGenericEmptyInput(t *testing.T) {
	res, err := (&Generic{}).Build(context.Background(), &obs.Input{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Checks) == 0 {
		t.Fatal("空输入也应有 event_gaps / log_unparsed 检查")
	}
}
