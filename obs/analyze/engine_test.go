package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func engineInput() *obs.Input {
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	return &obs.Input{
		Mode: obs.ModeSnapshot,
		Now:  now,
		Engine: obs.EngineState{Source: "engine", CapturedAt: now, ActivePools: 0,
			Sites: []obs.SiteState{{Key: "a", Paused: true, StageCount: 2}}},
		Events: []events.Event{
			{Type: events.TypeTaskRequeued, TS: now.Add(-5 * time.Minute), Queue: "recover_queue", Site: "a", Stage: "detail"},
		},
	}
}

func TestEngineSectionPausedAndRecover(t *testing.T) {
	res, err := (&EngineSection{Querier: &fakeQuerier{old: 379, sites: []obs.SiteState{{Key: "a", Paused: true}}}}).Build(context.Background(), engineInput())
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByID(res, "engine_paused"); c == nil || !c.Passed || c.Severity != obs.SeverityInfo {
		t.Fatalf("未知来源的暂停只应提供状态信息：%+v", c)
	}
	if c := checkByID(res, "recover_in_progress"); c == nil || !c.Passed {
		t.Fatalf("recover_in_progress 应通过（有 requeue + 旧 processing）：%+v", c)
	}
	// recover 与其它治理队列必须分开计数：旧实现把 repeat/error 队列的重投也算成 recover。
	if c := checkByID(res, "recover_in_progress"); c == nil ||
		!strings.Contains(c.Message, "recover 重投 1 次、其它队列重投 0 次") ||
		!strings.Contains(c.Message, "DB 旧 processing 379 条") {
		t.Fatalf("recover_in_progress 文案应分队列计数：%+v", c)
	}
	// 空闲时 engine_idle 应写空闲原因；passed 语义保持 true=空闲。
	if c := checkByID(res, "engine_idle"); c == nil || !c.Passed ||
		c.Message != "引擎空闲（无活跃阶段池，窗口内无任务完成/失败事件）" {
		t.Fatalf("空闲时 engine_idle 文案不对：%+v", c)
	}
	for _, id := range []string{"stage_pools", "engine_idle"} {
		if c := checkByID(res, id); c == nil {
			t.Fatalf("缺少检查 %s", id)
		}
	}
	if !strings.Contains(res.Summary, "来源 engine") {
		t.Fatalf("Summary 应说明引擎快照来源：%q", res.Summary)
	}
	joined := tableText(res)
	for _, want := range []string{"recover_queue", "DB 旧 processing", "站点状态"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("引擎段应包含 %q：%s", want, joined)
		}
	}
	// InProgress 是 submitted-completed-failed（含排队），列名必须写「未完成（含排队）」而不是「在跑」。
	if tbl := tableByTitle(res, "阶段池（窗口内最后一次快照）"); tbl == nil || !columnsContain(tbl.Columns, "未完成（含排队）") {
		t.Fatalf("阶段池第 3 列应改名为「未完成（含排队）」：%+v", tbl)
	}
}

// 注入的实时快照缺失时，站点表回落到 Querier 的 DB 快照。
func TestEngineSectionFallsBackToQuerierSites(t *testing.T) {
	in := engineInput()
	in.Engine.Sites = nil
	res, err := (&EngineSection{Querier: &fakeQuerier{sites: []obs.SiteState{{Key: "db-site", Paused: true}}}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tableText(res), "db-site") {
		t.Fatalf("站点快照应回落到 Querier：%s", tableText(res))
	}
	if c := checkByID(res, "engine_paused"); c == nil || !c.Passed || c.Severity != obs.SeverityInfo {
		t.Fatalf("DB 快照里来源未知的暂停不应告警：%+v", c)
	}
}

func TestEnginePauseSourceControlsSeverity(t *testing.T) {
	for _, tc := range []struct {
		source   string
		reason   string
		severity obs.Severity
		passed   bool
	}{
		{"manual", "人工维护", obs.SeverityInfo, true},
		{"startup", "站点声明 StartPaused", obs.SeverityInfo, true},
		{"", "旧快照未记录来源", obs.SeverityInfo, true},
		{"breaker", "窗口内终态失败数达到阈值", obs.SeverityWarn, false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			in := engineInput()
			in.Engine.Sites[0].PauseSource = tc.source
			in.Engine.Sites[0].PauseReason = tc.reason
			res, err := (&EngineSection{Querier: &fakeQuerier{}}).Build(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			c := checkByID(res, "engine_paused")
			if c == nil || c.Severity != tc.severity || c.Passed != tc.passed {
				t.Fatalf("pause source=%q: %+v", tc.source, c)
			}
			if !strings.Contains(tableText(res), tc.reason) {
				t.Fatalf("站点表丢失暂停原因: %s", tableText(res))
			}
			if tc.source != "" && !strings.Contains(tableText(res), tc.source) {
				t.Fatalf("站点表丢失暂停来源: %s", tableText(res))
			}
		})
	}
}

func TestEnginePauseFallbackPreservesSourceAndReason(t *testing.T) {
	in := engineInput()
	in.Engine.Sites = nil
	res, err := (&EngineSection{Querier: &fakeQuerier{sites: []obs.SiteState{
		{Key: "db-site", Paused: true, PauseSource: "manual", PauseReason: "运营检查"},
	}}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tableText(res), "manual") || !strings.Contains(tableText(res), "运营检查") {
		t.Fatalf("DB fallback 不应丢失暂停原因: %s", tableText(res))
	}
	if c := checkByID(res, "engine_paused"); c == nil || c.Severity != obs.SeverityInfo || !c.Passed {
		t.Fatalf("人工暂停 fallback 不应告警: %+v", c)
	}
}

// Querier 报错不拖垮本节：降级写进 in.Degraded，已拿到的数据照常出表。
func TestEngineSectionQuerierErrorDegrades(t *testing.T) {
	in := engineInput()
	in.Engine.Sites = nil
	res, err := (&EngineSection{Querier: &fakeQuerier{err: errors.New("boom")}}).Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Querier 报错不应让本节失败：%v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	var siteErr, oldErr bool
	for _, d := range in.Degraded {
		if strings.Contains(d, "站点快照查询失败") {
			siteErr = true
		}
		if strings.Contains(d, "旧 processing 查询失败") {
			oldErr = true
		}
	}
	if !siteErr || !oldErr {
		t.Fatalf("降级信息不全：%v", in.Degraded)
	}
}

// 非空闲时 engine_idle 不能再说「引擎空闲」：文案要给出活跃池与窗口内任务事件数。
// passed 语义不变（true=空闲），所以这里 !Passed 是对的。
func TestEngineIdleMessageWhenBusy(t *testing.T) {
	in := engineInput()
	now := in.Now
	in.Engine.ActivePools = 3
	in.Events = []events.Event{
		{Type: events.TypeTaskFinished, TS: now.Add(-2 * time.Minute)},
		{Type: events.TypeTaskFinished, TS: now.Add(-time.Minute)},
		{Type: events.TypeTaskFailed, TS: now.Add(-time.Minute)},
	}
	res, err := (&EngineSection{Querier: &fakeQuerier{}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(res, "engine_idle")
	if c == nil || c.Passed {
		t.Fatalf("引擎非空闲时 engine_idle 不应通过（passed 语义=true 表示空闲）：%+v", c)
	}
	if c.Message != "引擎非空闲（活跃阶段池 3，窗口内任务事件 3 条）" {
		t.Fatalf("非空闲文案不对：%q", c.Message)
	}
}

// recover 重投与其它治理队列（repeat/error）要分开计数，不能把周期重投算成启动恢复。
func TestEngineRecoverMessageSplitsQueues(t *testing.T) {
	in := engineInput()
	now := in.Now
	in.Events = []events.Event{
		{Type: events.TypeTaskRequeued, TS: now.Add(-40 * time.Minute), Queue: "recover_queue"},
		{Type: events.TypeTaskRequeued, TS: now.Add(-30 * time.Minute), Queue: "recover_queue"},
		{Type: events.TypeTaskRequeued, TS: now.Add(-20 * time.Minute), Queue: "repeat_queue"},
		{Type: events.TypeTaskRequeued, TS: now.Add(-10 * time.Minute), Queue: "error_queue"},
	}
	res, err := (&EngineSection{Querier: &fakeQuerier{old: 7}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(res, "recover_in_progress")
	if c == nil || !c.Passed {
		t.Fatalf("有旧 processing 时 recover_in_progress 应通过：%+v", c)
	}
	if c.Message != "窗口内 recover 重投 2 次、其它队列重投 2 次，DB 旧 processing 7 条" {
		t.Fatalf("recover_in_progress 分队列文案不对：%q", c.Message)
	}
}
