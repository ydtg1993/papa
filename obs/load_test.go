package obs

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

func TestFilterAndWindowHelpers(t *testing.T) {
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	evs := []events.Event{
		{Type: "a", RunID: "r1", TS: base},
		{Type: "b", RunID: "r1", TS: base.Add(time.Minute)},
		{Type: "c", RunID: "r2", TS: base.Add(2 * time.Minute)},
	}
	got := FilterRunID(evs, "r1")
	if len(got) != 2 {
		t.Fatalf("FilterRunID = %d", len(got))
	}
	from, to := EventWindow(evs)
	if !from.Equal(base) || !to.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("window = %v ~ %v", from, to)
	}
	if ids := CollectRunIDs(evs); len(ids) != 2 || ids[0] != "r1" || ids[1] != "r2" {
		t.Fatalf("run ids = %v", ids)
	}
}
