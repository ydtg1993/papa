package analyze

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"github.com/ydtg1993/papa/v4/pkg/incident"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

func TestBreakerReportLoadsSilentActiveIncidentWithoutLogEvents(t *testing.T) {
	f := gormfake.New()
	l := incident.Transition{Action: "manual", To: "manual", At: time.Now().Add(-48 * time.Hour), Reason: "持续保护"}
	payload, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	f.SetRow(map[string]driver.Value{"incident_id": "yesterday", "site": "fixture", "state": "manual", "sequence": int64(1), "at": l.At, "payload": payload})
	res := &obs.SectionResult{}
	addBreakerReports(context.Background(), &obs.Input{DB: gormfake.MustOpen(f)}, res)
	if len(res.Tables) != 1 || len(res.Tables[0].Rows) != 1 || res.Tables[0].Rows[0][0] != "yesterday" {
		t.Fatalf("silent active report missing: %+v", res)
	}
}

func TestBreakerReportReferencesDurableIDWithoutReconstruction(t *testing.T) {
	in := &obs.Input{Events: []events.Event{
		{Type: events.TypeTaskFailed, Site: "a", Error: "must not reconstruct"},
		{Type: events.TypeBreakerTripped, Site: "a", Attrs: map[string]string{"incident_id": "durable-one"}},
		{Type: events.TypeBreakerTransition, Site: "a", Attrs: map[string]string{"incident_id": "observed-without-pause"}},
		{Type: events.TypeBreakerTripped, Site: "a", Attrs: map[string]string{"incident_id": "durable-one"}},
	}}
	res := &obs.SectionResult{}
	addBreakerReports(context.Background(), in, res)
	if len(res.Tables) != 1 || len(res.Tables[0].Rows) != 2 || res.Tables[0].Rows[0][0] != "durable-one" || res.Tables[0].Rows[1][0] != "observed-without-pause" {
		t.Fatalf("references: %+v", res)
	}
	if len(res.Problems) != 0 || !strings.Contains(res.Tables[0].Rows[0][1], "未连接数据库") {
		t.Fatal("offline reports must disclose missing data instead of fabricating evidence")
	}
}

func TestBreakerReviewRecentTransitionMustKeepOlderIncidentReference(t *testing.T) {
	in := &obs.Input{}
	for i := 0; i < 33; i++ {
		id := "incident-" + string(rune('A'+i))
		in.Events = append(in.Events, events.Event{Type: events.TypeBreakerTransition, Attrs: map[string]string{"incident_id": id}})
	}
	in.Events = append(in.Events, events.Event{Type: events.TypeBreakerTransition, Attrs: map[string]string{"incident_id": "incident-A", "state": "manual"}})
	res := &obs.SectionResult{}
	addBreakerReports(context.Background(), in, res)
	for _, table := range res.Tables {
		for _, row := range table.Rows {
			if row[0] == "incident-A" {
				return
			}
		}
	}
	t.Fatal("most recently updated incident silently omitted because its first occurrence was oldest")
}
