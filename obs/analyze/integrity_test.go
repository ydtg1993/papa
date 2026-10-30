package analyze

import (
	"context"
	"strings"
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
	// 断言具体的降级项，而不是「有任意 degraded」：无 DB 时 framework state 也会写 degraded，
	// 只查 len>0 会掩盖「真断层没写 degraded」。
	if !degradedContains(in.Degraded, "事件存在时间断层") {
		t.Fatalf("真断层应写入事件断层 degraded：%v", in.Degraded)
	}
}

// 最大断层与一段停机（run_stopped↔run_started）秒级吻合时：不是丢事件，应判通过且不写 degraded。
func TestEventCoverageDowntimeIsNotDataLoss(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	stopped := now.Add(-12 * time.Hour)
	restarted := now.Add(-2*time.Hour - 13*time.Minute)
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now,
		Run: obs.RunMeta{From: stopped.Add(-5 * time.Minute), To: now},
		Events: []events.Event{
			{Type: events.TypeRunStarted, TS: stopped.Add(-5 * time.Minute)},
			{Type: events.TypeTaskFinished, TS: stopped},
			{Type: events.TypeRunStopped, TS: stopped},
			{Type: events.TypeRunStarted, TS: restarted},
			{Type: events.TypeTaskFinished, TS: restarted.Add(time.Minute)},
		}}
	out, err := (&Generic{Thresholds: Thresholds{EventMaxGap: 10 * time.Minute}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(out, "event_coverage")
	if c == nil || !c.Passed {
		t.Fatalf("停机造成的断层应判通过：%+v", c)
	}
	if !strings.Contains(c.Message, "引擎停机") || !strings.Contains(c.Message, "不是事件丢失") {
		t.Fatalf("message 应说明是停机而不是事件丢失：%q", c.Message)
	}
	if degradedContains(in.Degraded, "事件") {
		t.Fatalf("停机不应写事件口径的 degraded：%v", in.Degraded)
	}
}

// 停机之外还有更大的真断层时仍要报：不能因为窗口里有停机就把所有断层都当正常。
func TestEventCoverageRealGapLargerThanDowntimeStillWarns(t *testing.T) {
	now := time.Date(2026, 10, 29, 14, 0, 0, 0, time.Local)
	stopped := now.Add(-10 * time.Hour)
	restarted := stopped.Add(20 * time.Minute) // 停机 20m（不是最大断层）
	in := &obs.Input{Mode: obs.ModeAcceptance, Now: now,
		Run: obs.RunMeta{From: stopped.Add(-5 * time.Minute), To: now},
		Events: []events.Event{
			{Type: events.TypeRunStarted, TS: stopped.Add(-5 * time.Minute)},
			{Type: events.TypeTaskFinished, TS: stopped},
			{Type: events.TypeRunStopped, TS: stopped},
			{Type: events.TypeRunStarted, TS: restarted},
			{Type: events.TypeTaskFinished, TS: restarted.Add(time.Minute)},
			// 停机之后又有 2h 没事件：这才是真断层，比停机（20m）大。
			{Type: events.TypeTaskFinished, TS: restarted.Add(2*time.Hour + time.Minute)},
		}}
	out, err := (&Generic{Thresholds: Thresholds{EventMaxGap: 10 * time.Minute}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(out, "event_coverage")
	if c == nil || c.Passed {
		t.Fatalf("停机之外的真断层仍应未通过：%+v", c)
	}
	if !degradedContains(in.Degraded, "事件存在时间断层") {
		t.Fatalf("真断层应写入 degraded：%v", in.Degraded)
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

func degradedContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
