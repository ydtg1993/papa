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
