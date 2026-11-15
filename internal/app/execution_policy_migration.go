package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/breakerstore"
	"github.com/ydtg1993/papa/v4/models"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MigrateExecutionPolicySnapshots is an explicit offline upgrade. oldDoc must be
// the operator-confirmed effective pre-upgrade policy (including site overrides),
// not the desired replacement policy. Stop all collectors before calling it.
// It preserves every row's identity, control revision, budget, window and report;
// only the persisted control format and frozen policy fields change.
func (a *App) MigrateExecutionPolicySnapshots(ctx context.Context, confirmedRevision uint64, oldDoc ep.Document) error {
	if a.DB == nil {
		return fmt.Errorf("breaker policy migration requires database")
	}
	if a.executionPolicy != nil {
		return fmt.Errorf("breaker policy migration is offline-only; stop runtime before migration")
	}
	if confirmedRevision == 0 {
		return fmt.Errorf("confirmed old policy revision must be positive")
	}
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []models.BreakerIncident
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Order("id ASC").Find(&rows).Error; err != nil {
			return err
		}
		// Historical sites may no longer be registered. They still require explicit
		// policy provenance and remain readable in the incident archive after upgrade.
		sites := make([]string, 0, len(rows))
		for _, row := range rows {
			sites = append(sites, row.Site)
		}
		if a.Engine != nil {
			sites = append(sites, a.executionSites()...)
		}
		if err := oldDoc.Validate(sites); err != nil {
			return err
		}
		for _, row := range rows {
			payload, changed, err := migrateBreakerPolicyRow(row, confirmedRevision, oldDoc)
			if err != nil {
				return fmt.Errorf("breaker incident %s: %w", row.ID, err)
			}
			if !changed {
				continue
			}
			// A transaction and row lock preserve metadata and prevent stale overwrites.
			q := tx.Model(&models.BreakerIncident{}).Where("id = ? AND revision = ?", row.ID, row.Revision).UpdateColumn("payload", payload)
			if q.Error != nil {
				return q.Error
			}
			if q.RowsAffected != 1 {
				return fmt.Errorf("breaker incident %s changed during migration", row.ID)
			}
		}
		return nil
	})
}

func migrateBreakerPolicyRow(row models.BreakerIncident, revision uint64, doc ep.Document) ([]byte, bool, error) {
	if len(row.Payload) > 16<<20 {
		return nil, false, fmt.Errorf("snapshot exceeds payload budget")
	}
	var saved breakerstore.Snapshot
	decoder := json.NewDecoder(bytes.NewReader(row.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return nil, false, err
	}
	c := saved.Control
	if c.ID != row.ID || c.Site != row.Site || c.Stage != row.Stage || c.State != row.State || c.Revision != row.Revision || c.Active() != row.Active {
		return nil, false, fmt.Errorf("snapshot metadata mismatch")
	}
	if c.Version == 2 {
		return nil, false, saved.ValidateCurrent()
	}
	native := doc.Breaker.Defaults.Native()
	if override, ok := doc.Breaker.Sites[row.Site]; ok {
		native = override.Native()
	}
	migrated, err := breakerstore.MigratePolicySnapshot(saved, revision, breaker.Config{Enabled: native.Enabled, Window: native.Window, Threshold: native.Threshold, Policy: native.Policy})
	if err != nil {
		return nil, false, err
	}
	// Keep the original JSON object, including the complete display report, so
	// migration cannot apply new redaction/truncation to historical evidence.
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(row.Payload, &envelope); err != nil {
		return nil, false, err
	}
	var control map[string]json.RawMessage
	if err = json.Unmarshal(envelope["control"], &control); err != nil {
		return nil, false, err
	}
	control["Version"], _ = json.Marshal(migrated.Control.Version)
	control["PolicyRevision"], _ = json.Marshal(migrated.Control.PolicyRevision)
	control["FrozenPolicy"], _ = json.Marshal(migrated.Control.FrozenPolicy)
	envelope["control"], err = json.Marshal(control)
	if err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, false, err
	}
	var validated breakerstore.Snapshot
	if err = json.Unmarshal(payload, &validated); err != nil {
		return nil, false, err
	}
	if err = validated.ValidateCurrent(); err != nil {
		return nil, false, err
	}
	return payload, true, nil
}
