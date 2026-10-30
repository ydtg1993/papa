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

// InProgress 本身已包含排队（workerpool.Stats = submitted-completed-failed），
// queue_drained 只能累加 InProgress；旧实现再加 QueueLen 会把排队算两遍（10+12=22 而不是 12）。
func TestQueueDrainedCountsOutstandingOnce(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	in := &obs.Input{Now: now, Run: obs.RunMeta{From: now.Add(-time.Hour), To: now},
		Events: []events.Event{{
			Type: events.TypeStageSnapshot, TS: now.Add(-time.Minute),
			Stages: map[string]events.StageSnapshot{
				"s:catalog": {Site: "s", Stage: "catalog", QueueLen: 10, InProgress: 12, Workers: 2},
			},
		}}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(res, "queue_drained")
	if c == nil || c.Passed {
		t.Fatalf("有未完成任务时 queue_drained 应未通过：%+v", c)
	}
	if !strings.Contains(c.Message, "未完成 12") {
		t.Fatalf("queue_drained message 应只算 InProgress=12，不能 10+12=22：%q", c.Message)
	}
	tbl := tableByTitle(res, "队列快照（窗口内最后一次）")
	if tbl == nil || !columnsContain(tbl.Columns, "未完成（含排队）") {
		t.Fatalf("队列快照第 3 列应改名为「未完成（含排队）」：%+v", tbl)
	}
}

// spill（引擎内存溢出表）不在内存队列里，queue_drained 必须把最后一条 spill_changed 的 backlog 计入。
func TestQueueDrainedIncludesSpillBacklog(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	in := &obs.Input{Now: now, Run: obs.RunMeta{From: now.Add(-time.Hour), To: now},
		Events: []events.Event{
			{
				Type: events.TypeStageSnapshot, TS: now.Add(-2 * time.Minute),
				Stages: map[string]events.StageSnapshot{
					"s:catalog": {Site: "s", Stage: "catalog", QueueLen: 1, InProgress: 3, Workers: 1},
				},
			},
			{Type: events.TypeSpillChanged, TS: now.Add(-time.Minute),
				Counts: map[string]int{"drained": 7}, Backlog: 5},
		}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(res, "queue_drained")
	if c == nil || c.Passed {
		t.Fatalf("未完成 3 + spill 5 应判未通过：%+v", c)
	}
	for _, want := range []string{"未完成 3", "spill 5"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("queue_drained message 缺 %q：%q", want, c.Message)
		}
	}
	spill := tableByTitle(res, "溢出表（spill，引擎内存）")
	if spill == nil || len(spill.Rows) != 1 {
		t.Fatalf("应输出溢出表：%+v", spill)
	}
	if got := strings.Join(spill.Rows[0], " | "); !strings.Contains(got, "7") || !strings.Contains(got, "5") {
		t.Fatalf("溢出表应含最近回灌 7 / 当前积压 5：%s", got)
	}
}

// 同一失败组里两个不同任务时，证据要分别列出 task_id，不能写成「首条 task_id 失败 N 次」。
func TestFailureEvidenceListsDistinctTasks(t *testing.T) {
	now := time.Date(2026, 10, 29, 13, 0, 0, 0, time.Local)
	in := &obs.Input{Now: now, Run: obs.RunMeta{From: now.Add(-2 * time.Hour), To: now},
		Events: []events.Event{
			{Type: events.TypeTaskFailed, TS: now.Add(-time.Hour), Site: "hgdju", Stage: "catalog", Kind: "structure",
				TaskID: 9676, URL: "https://hgdju.com/browse/page-2", Error: "unsupported detail route A"},
			{Type: events.TypeTaskFailed, TS: now.Add(-30 * time.Minute), Site: "hgdju", Stage: "catalog", Kind: "structure",
				TaskID: 9678, URL: "https://hgdju.com/browse/page-33", Error: "unsupported detail route B"},
		}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var found *obs.Problem
	for i := range res.Problems {
		if res.Problems[i].ID == "task-failed-hgdju-catalog-structure" {
			found = &res.Problems[i]
		}
	}
	if found == nil || len(found.Evidence) == 0 {
		t.Fatalf("应有 hgdju/catalog 失败分组问题：%+v", res.Problems)
	}
	ref := found.Evidence[0].Ref
	for _, want := range []string{"失败任务 2 个", "9676@", "9678@", "（共 2 次）"} {
		if !strings.Contains(ref, want) {
			t.Fatalf("失败证据应包含 %q，实得 %q", want, ref)
		}
	}
	if strings.Contains(ref, "task_id=") {
		t.Fatalf("不应再写旧的「task_id=首条」措辞：%q", ref)
	}
}

// crawl_fail_rate 是「事件尝试口径」，文案不能再说「终态失败率」。
func TestCrawlFailRateMessageIsAttemptScope(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	in := &obs.Input{Now: now, Run: obs.RunMeta{From: now.Add(-time.Hour), To: now},
		Events: []events.Event{
			{Type: events.TypeTaskFinished, TS: now.Add(-time.Minute), Site: "a", Stage: "detail"},
			{Type: events.TypeTaskFailed, TS: now.Add(-time.Minute), Site: "a", Stage: "detail", Kind: "structure"},
		}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(res, "crawl_fail_rate")
	if c == nil {
		t.Fatal("缺少 crawl_fail_rate")
	}
	for _, want := range []string{"抓取失败事件率", "次尝试"} {
		if !strings.Contains(c.Message, want) {
			t.Fatalf("message 缺 %q：%q", want, c.Message)
		}
	}
	if !strings.Contains(c.Detail, "终态失败") {
		t.Fatalf("detail 应说明终态失败看任务终态分布：%q", c.Detail)
	}
}

// 运行边界要给出「有效运行时长（剔除停机）」，口径与吞吐/ETA 的分母一致。
func TestRuntimeTableShowsEffectiveDuration(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	from := now.Add(-10 * time.Hour)
	stopped := from.Add(2 * time.Hour)
	restarted := from.Add(8 * time.Hour) // 停机 6h → 有效运行 4h
	in := &obs.Input{Now: now, Run: obs.RunMeta{From: from, To: now},
		Events: []events.Event{
			{Type: events.TypeRunStarted, TS: from},
			{Type: events.TypeRunStopped, TS: stopped},
			{Type: events.TypeRunStarted, TS: restarted},
			{Type: events.TypeTaskFinished, TS: restarted.Add(time.Hour), Site: "a", Stage: "detail"},
		}}
	res, err := (&Generic{}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	tbl := tableByTitle(res, "运行边界")
	if tbl == nil {
		t.Fatal("缺少运行边界表")
	}
	var value string
	for _, row := range tbl.Rows {
		if len(row) == 2 && row[0] == "有效运行时长（剔除停机）" {
			value = row[1]
		}
	}
	if value != "4h0m0s（剔除停机 6h0m0s）" {
		t.Fatalf("有效运行时长 = %q, want 4h0m0s（剔除停机 6h0m0s）", value)
	}
}
