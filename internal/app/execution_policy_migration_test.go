package app

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/models"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"gorm.io/datatypes"
	"reflect"
	"testing"
	"time"
)

func legacyPolicyRow(t *testing.T, id, state string) models.BreakerIncident {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	s := breakerstore.Snapshot{Control: breaker.Recovery{Version: 1, ID: id, Site: "retired", State: state, Revision: 7, ProbeAttempts: 2, Cycle: 4, Deadline: now.Add(time.Minute), StartedAt: now, UpdatedAt: now}, Window: &breaker.Checkpoint{Failures: []breaker.Outcome{{TaskID: 42, At: now}}}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return models.BreakerIncident{ID: id, Site: "retired", State: state, Revision: 7, Active: s.Control.Active(), StartedAt: now, UpdatedAt: now, Payload: datatypes.JSON(raw)}
}
func TestExecutionPolicyMigrationPreservesRowsAndRejectsUnknownFormat(t *testing.T) {
	d := ep.InitialDocument()
	override := d.Breaker.Defaults
	override.Threshold = 73
	d.Breaker.Sites["retired"] = override
	for _, state := range []string{"manual", "closed"} {
		row := legacyPolicyRow(t, state, state)
		raw, changed, err := migrateBreakerPolicyRow(row, 9, d)
		if err != nil || !changed {
			t.Fatalf("migration: %v %v", changed, err)
		}
		var before, after breakerstore.Snapshot
		if err = json.Unmarshal(row.Payload, &before); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &after); err != nil {
			t.Fatal(err)
		}
		if after.Control.FrozenPolicy.Threshold != 73 || after.Control.PolicyRevision != 9 {
			t.Fatal("wrong site provenance")
		}
		after.Control.Version = before.Control.Version
		after.Control.PolicyRevision = 0
		after.Control.FrozenPolicy = breaker.Config{}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("migration changed historical state")
		}
		row.Payload = raw
		if _, changed, err = migrateBreakerPolicyRow(row, 9, d); err != nil || changed {
			t.Fatalf("idempotent migration: %v %v", changed, err)
		}
	}
	row := legacyPolicyRow(t, "bad", "manual")
	row.Site = "wrong"
	if _, _, err := migrateBreakerPolicyRow(row, 9, d); err == nil {
		t.Fatal("mismatched metadata accepted")
	}
}
func TestExecutionPolicyMigrationMySQLRollsBackAllRows(t *testing.T) {
	db, _ := appMixDatabase(t, false)
	a := &App{DB: db}
	ctx := context.Background()
	doc := ep.InitialDocument()
	first := legacyPolicyRow(t, "a-active", "manual")
	second := legacyPolicyRow(t, "z-closed", "closed")
	var corrupt map[string]any
	if err := json.Unmarshal(second.Payload, &corrupt); err != nil {
		t.Fatal(err)
	}
	corrupt["control"].(map[string]any)["Version"] = 0
	broken, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	second.Payload = broken
	if err = db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err = a.MigrateExecutionPolicySnapshots(ctx, 3, doc); err == nil {
		t.Fatal("bad second row accepted")
	}
	var stored models.BreakerIncident
	if err = db.Where("id = ?", first.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var snapshot breakerstore.Snapshot
	if err = json.Unmarshal(stored.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Control.Version != 1 {
		t.Fatal("first row committed despite later failure")
	}
	fixed := legacyPolicyRow(t, second.ID, "closed")
	if err = db.Model(&models.BreakerIncident{}).Where("id = ?", second.ID).UpdateColumn("payload", fixed.Payload).Error; err != nil {
		t.Fatal(err)
	}
	if err = a.MigrateExecutionPolicySnapshots(ctx, 3, doc); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		stored = models.BreakerIncident{}
		if err = db.Where("id = ?", id).Take(&stored).Error; err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(stored.Payload, &snapshot); err != nil {
			t.Fatal(err)
		}
		if err = snapshot.ValidateCurrent(); err != nil {
			t.Fatal(err)
		}
		if stored.Revision != 7 || snapshot.Control.ProbeAttempts != 2 || snapshot.Control.Cycle != 4 {
			t.Fatal("migration reset budgets")
		}
	}
	if err = a.MigrateExecutionPolicySnapshots(ctx, 3, doc); err != nil {
		t.Fatal("retry", err)
	}
}
