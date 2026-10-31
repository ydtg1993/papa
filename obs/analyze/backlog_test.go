package analyze

import (
	"context"
	"math"
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
	// 通过时文案必须写吞吐实况，不能仍写「窗口内 0 吞吐」（旧实现无条件写失败句式）。
	if c := checkByID(res, "throughput_zero"); c == nil || c.Message != "窗口内吞吐 0.15 条/分（非轮询待处理 60 条）" {
		t.Fatalf("throughput_zero 通过时应写实况吞吐：%+v", c)
	}
}

// 0 吞吐且确实有待处理时才保留失败句式。
func TestThroughputZeroFailureMessage(t *testing.T) {
	in := backlogInput()
	in.Events = nil // 窗口内没有任何完成/失败事件 → 吞吐 0
	q := &fakeQuerier{bucket: []TaskBucket{
		{Site: "a", Stage: "detail", Status: 0, Repeatable: 0, AgeBucket: "lt10m", N: 60},
	}}
	res, err := (&BacklogSection{Querier: q}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(res, "throughput_zero")
	if c == nil || c.Passed || !c.Skipped {
		t.Fatalf("有非轮询待处理且 0 吞吐时缺少运行证据应为 skipped：%+v", c)
	}
	if c.Message != "有 60 条非轮询待处理但窗口内 0 吞吐" {
		t.Fatalf("throughput_zero 失败文案不对：%q", c.Message)
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

// 吞吐分母用「窗口 - 停机」：4h 窗口里停机 2h，60 条完成应算 0.5/min（旧口径 60/240=0.25）。
func TestBacklogThroughputUsesEffectiveRuntime(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	from := now.Add(-4 * time.Hour)
	stopped := from.Add(time.Hour)
	restarted := stopped.Add(2 * time.Hour) // 停机 2h → 有效运行 2h
	evs := []events.Event{
		{Type: events.TypeRunStarted, TS: from},
		{Type: events.TypeRunStopped, TS: stopped},
		{Type: events.TypeRunStarted, TS: restarted},
		{Type: events.TypeRunStopped, TS: now},
	}
	for i := 0; i < 60; i++ {
		evs = append(evs, events.Event{Type: events.TypeTaskFinished, TS: restarted.Add(time.Duration(i) * time.Second), Site: "a", Stage: "detail"})
	}
	q := &fakeQuerier{bucket: []TaskBucket{{Site: "a", Stage: "detail", Status: 0, Repeatable: 0, AgeBucket: "lt10m", N: 120}}}
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now,
		Run: obs.RunMeta{From: from, To: now}, Events: evs}
	res, err := (&BacklogSection{Querier: q}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics.(obs.BacklogSummary)
	if math.Abs(m.ThroughputPerMin-0.5) > 1e-9 {
		t.Fatalf("吞吐 = %v, want 0.5 条/分（有效运行 2h 而不是窗口 4h）：%+v", m.ThroughputPerMin, m)
	}
	for _, want := range []string{"观测运行（含暂停） 2h0m0s", "已识别停机 2h0m0s"} {
		if !strings.Contains(res.Summary, want) {
			t.Fatalf("summary 缺 %q：%s", want, res.Summary)
		}
	}
}

// 摘要 ETA 用整体口径（非轮询待处理总量 ÷ 总吞吐），不是「循环里最后一行的 ETA」。
func TestBacklogSummaryETAUsesTotalThroughput(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	from := now.Add(-time.Hour)
	evs := []events.Event{{Type: events.TypeStageSnapshot, TS: now}}
	for i := 0; i < 30; i++ {
		evs = append(evs, events.Event{Type: events.TypeTaskFinished, TS: from.Add(time.Duration(i) * time.Minute), Site: "a", Stage: "detail"})
	}
	for i := 0; i < 10; i++ {
		evs = append(evs, events.Event{Type: events.TypeTaskFinished, TS: from.Add(time.Duration(i) * time.Minute), Site: "b", Stage: "detail"})
	}
	q := &fakeQuerier{bucket: []TaskBucket{
		{Site: "a", Stage: "detail", Status: 0, Repeatable: 0, AgeBucket: "lt10m", N: 60},
		{Site: "b", Stage: "detail", Status: 0, Repeatable: 0, AgeBucket: "lt10m", N: 60},
	}}
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now,
		Run: obs.RunMeta{From: from, To: now}, Events: evs}
	res, err := (&BacklogSection{Querier: q}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Metrics.(obs.BacklogSummary)
	// 总吞吐 = 30/60 + 10/60 = 0.6667/min；120 条待处理 → 180min = 3h。
	// 旧实现取排序最后一行 b 的 ETA：60/(10/60)=360min → 「约 6.0 小时」。
	if m.ETA != "约 3.0 小时" {
		t.Fatalf("摘要 ETA = %q, want 约 3.0 小时（整体口径）：%+v", m.ETA, m)
	}
	// per-stage 表格的 ETA 仍逐行保留：a 约 2h、b 约 6h。
	text := tableText(res)
	for _, want := range []string{"约 2.0 小时", "约 6.0 小时"} {
		if !strings.Contains(text, want) {
			t.Fatalf("per-stage 表格应保留 %q：\n%s", want, text)
		}
	}
}
