package breakerstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
)

func TestBreakerHistoryMySQLAppendRetryAndHistoricalDay(t *testing.T) {
	db := isolatedMySQL(t)
	store := New(db)
	ctx := context.Background()
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	snapshot := LegacySnapshot(incident.Lifecycle{Capture: incident.Capture{ID: "history", Site: "synthetic"}, Revision: 1, State: "manual", StartedAt: day.Add(-time.Hour), UpdatedAt: day, NextProbeAt: day.Add(time.Hour)})
	for i := 0; i < 141; i++ {
		at := day.Add(time.Duration(i) * time.Minute)
		if i == 0 {
			at = day.Add(-time.Hour)
		}
		snapshot.History = append(snapshot.History, incident.Transition{At: at, Sequence: uint64(i + 1), Action: "hourly_recheck", To: "manual", Reason: fmt.Sprintf("record-%d", i), Check: &incident.StageCheck{Stage: "detail", Verification: "readonly", Coverage: "original-scope", State: "pending", RequiredProbes: 3}})
	}
	snapshot.Control.HistorySequence = 141
	snapshot.Lifecycle.Timeline = snapshot.History
	if err := store.Save(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, snapshot); err != nil {
		t.Fatal("lost ack retry", err)
	}
	var count int64
	if err := db.Model(&models.BreakerHistory{}).Count(&count).Error; err != nil || count != 141 {
		t.Fatalf("history duplicate/loss %d %v", count, err)
	}
	next := snapshot
	next.Control.Revision = 2
	next.Lifecycle.Revision = 2
	next.Control.State = "recovered"
	next.Lifecycle.State = "recovered"
	next.Control.HistorySequence = 142
	next.History = []incident.Transition{{At: day.Add(25 * time.Hour), Sequence: 142, Action: "recovered", To: "recovered", Reason: "tomorrow-recovery"}}
	if err := store.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
	rows, limited, err := store.HistoryForWindow(ctx, day, day.Add(24*time.Hour), 10000)
	if err != nil || limited || len(rows) != 141 {
		t.Fatalf("day lost old entries: %d %v %v", len(rows), limited, err)
	}
	for _, row := range rows {
		text, err := HistoryText(row)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == "recovered" || strings.Contains(text, "tomorrow-recovery") || !strings.Contains(text, "original-scope") {
			t.Fatal("historical day used future/current snapshot", text)
		}
	}
	rows, _, err = store.HistoryForWindow(ctx, day.Add(24*time.Hour), day.Add(48*time.Hour), 10000)
	if err != nil || len(rows) != 2 || rows[0].Sequence != 141 || rows[1].Sequence != 142 {
		t.Fatalf("cross-day baseline missing: %+v %v", rows, err)
	}
	rows, _, err = store.HistoryForWindow(ctx, day.Add(48*time.Hour), day.Add(72*time.Hour), 10000)
	if err != nil || len(rows) != 0 {
		t.Fatal("closed event shown as still active next day")
	}
}

func TestBreakerHistoryMySQLFailureRollsBackControl(t *testing.T) {
	db := isolatedMySQL(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now()
	snapshot := LegacySnapshot(incident.Lifecycle{Capture: incident.Capture{ID: "atomic", Site: "synthetic"}, Revision: 1, State: "cooldown", StartedAt: now, UpdatedAt: now})
	snapshot.Control.HistorySequence = 1
	snapshot.History = []incident.Transition{{Sequence: 1, At: now, To: "cooldown", Action: "cooldown"}}
	if err := store.Save(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Control.Revision = 2
	snapshot.Lifecycle.Revision = 2
	snapshot.History = []incident.Transition{{Sequence: 0, At: now, To: "ramping"}}
	if err := store.Save(ctx, snapshot); err == nil {
		t.Fatal("invalid history accepted")
	}
	var row models.BreakerIncident
	if err := db.First(&row, "id = ?", "atomic").Error; err != nil {
		t.Fatal(err)
	}
	if row.Revision != 1 {
		t.Fatal("control committed without history")
	}
}
