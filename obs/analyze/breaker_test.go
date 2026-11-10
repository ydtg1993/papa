package analyze

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

func TestBreakerReportUsesIncidentWindowAndRun(t *testing.T) {
	now := time.Now()
	failure := func(seq uint64, run, site, msg string, at time.Time) events.Event {
		return events.Event{Type: events.TypeTaskFailed, Seq: seq, RunID: run, Site: site, Stage: "detail", TS: at, Error: msg, TaskID: 1}
	}
	in := &obs.Input{Events: []events.Event{
		failure(1, "old", "a", "wrong run", now), failure(2, "run", "b", "wrong site", now),
		failure(3, "run", "a", "before resume", now.Add(-30*time.Second)), failure(4, "run", "a", "HTTP 429", now.Add(-time.Second)),
		{Type: events.TypeBreakerTripped, Seq: 5, RunID: "run", Site: "a", TS: now, Counts: map[string]int{"failures": 1, "threshold": 1}, Attrs: map[string]string{"window_ns": strconv.FormatInt(int64(time.Minute), 10), "window_from": now.Add(-10 * time.Second).Format(time.RFC3339Nano)}},
		failure(6, "run", "a", "after trip", now.Add(time.Second)),
	}}
	res := &obs.SectionResult{}
	addBreakerIncidents(in, res)
	if len(res.Problems) != 1 {
		t.Fatalf("problems: %+v", res.Problems)
	}
	if len(res.Tables) != 1 || len(res.Tables[0].Rows) != 1 || res.Tables[0].Rows[0][8] != "HTTP 429" {
		t.Fatalf("human-readable report lost grouped evidence: %+v", res.Tables)
	}
	text := res.Problems[0].Evidence[0].Excerpt
	if !strings.Contains(text, "HTTP 429") {
		t.Fatal("missing trigger evidence")
	}
	for _, bad := range []string{"wrong run", "wrong site", "before resume", "after trip"} {
		if strings.Contains(text, bad) {
			t.Fatalf("wrong window: %s", text)
		}
	}
	in.Events = []events.Event{{Type: events.TypeBreakerTripped, Site: "a", TS: now}}
	res = &obs.SectionResult{}
	addBreakerIncidents(in, res)
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0].Title, "缺少统计窗口") {
		t.Fatal("legacy event must disclose unavailable evidence")
	}
}
