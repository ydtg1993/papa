package obs

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

func TestComputeEventCoverage(t *testing.T) {
	base := time.Date(2026, 10, 29, 0, 0, 0, 0, time.Local)
	evs := []events.Event{
		{Type: events.TypeRunStarted, TS: base},
		{Type: events.TypeTaskFinished, TS: base.Add(5 * time.Minute)},
		{Type: events.TypeTaskFinished, TS: base.Add(95 * time.Minute)}, // 90m 断层
	}
	c := ComputeEventCoverage(evs)
	if !c.FirstTS.Equal(base) || !c.LastTS.Equal(base.Add(95*time.Minute)) {
		t.Fatalf("首末事件不对：%+v", c)
	}
	if c.MaxGap != 90*time.Minute {
		t.Fatalf("MaxGap = %v, want 90m", c.MaxGap)
	}
}

// run_stopped 与下一次 run_started 配成一段停机；其余事件不参与配对。
func TestComputeDowntimesPairsStopAndStart(t *testing.T) {
	base := time.Date(2026, 10, 29, 0, 0, 0, 0, time.Local)
	stopped := base.Add(2 * time.Hour)
	restarted := base.Add(11 * time.Hour)
	evs := []events.Event{
		{Type: events.TypeRunStarted, TS: base},
		{Type: events.TypeTaskFinished, TS: base.Add(time.Hour)},
		{Type: events.TypeRunStopped, TS: stopped},
		{Type: events.TypeRunStarted, TS: restarted},
		{Type: events.TypeTaskFinished, TS: restarted.Add(time.Minute)},
	}
	dts := ComputeDowntimes(evs, restarted.Add(2*time.Hour))
	if len(dts) != 1 {
		t.Fatalf("停机区间数 = %d, want 1：%+v", len(dts), dts)
	}
	if !dts[0].From.Equal(stopped) || !dts[0].To.Equal(restarted) {
		t.Fatalf("停机区间 = %v ~ %v, want %v ~ %v", dts[0].From, dts[0].To, stopped, restarted)
	}
	if got := TotalDowntime(dts, base, restarted.Add(2*time.Hour)); got != restarted.Sub(stopped) {
		t.Fatalf("TotalDowntime = %v, want %v", got, restarted.Sub(stopped))
	}
	// 窗口只覆盖停机的前半段：累计值必须按窗口裁剪，不能把窗口外的停机也算进来。
	if got := TotalDowntime(dts, base, stopped.Add(time.Hour)); got != time.Hour {
		t.Fatalf("裁剪后的 TotalDowntime = %v, want 1h", got)
	}
}

// 末尾未闭合的 run_stopped（报告时进程已停、还没重启）一直算到窗口末。
func TestComputeDowntimesUnclosedStopRunsTo(t *testing.T) {
	base := time.Date(2026, 10, 29, 0, 0, 0, 0, time.Local)
	stopped := base.Add(time.Hour)
	to := base.Add(3 * time.Hour)
	evs := []events.Event{
		{Type: events.TypeRunStarted, TS: base},
		{Type: events.TypeRunStopped, TS: stopped},
	}
	dts := ComputeDowntimes(evs, to)
	if len(dts) != 1 || !dts[0].From.Equal(stopped) || !dts[0].To.Equal(to) {
		t.Fatalf("未闭合的 run_stopped 应算到 to：%+v (to=%v)", dts, to)
	}
}

// 只有 run_started（起于窗口前，或窗口从运行中途开始）时不能凭空造停机区间。
func TestComputeDowntimesIgnoresStartWithoutStop(t *testing.T) {
	base := time.Date(2026, 10, 29, 0, 0, 0, 0, time.Local)
	evs := []events.Event{
		{Type: events.TypeRunStarted, TS: base},
		{Type: events.TypeTaskFinished, TS: base.Add(time.Hour)},
	}
	if dts := ComputeDowntimes(evs, base.Add(2*time.Hour)); len(dts) != 0 {
		t.Fatalf("没有 run_stopped 不应还原出停机区间：%+v", dts)
	}
}
