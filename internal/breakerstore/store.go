// Package breakerstore persists bounded circuit-breaker lifecycle snapshots.
package breakerstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrStale = errors.New("breaker event snapshot is stale or conflicting")

const MaxActive = 1024
const maxPayload = 16 << 20

type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

// Save is idempotent for identical revisions; an older or conflicting snapshot
// never replaces a newer report. Caller owns visible retry on persistence errors.
func (s *Store) Save(ctx context.Context, snapshot Snapshot) error {
	if s == nil || s.db == nil {
		return errors.New("breaker store is not configured")
	}
	if err := snapshot.ValidateCurrent(); err != nil {
		return err
	}
	snapshot = snapshot.Clean()
	l := snapshot.Control
	if l.ID == "" || l.Site == "" || l.Revision == 0 {
		return errors.New("breaker event needs ID, site and positive revision")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(payload) > maxPayload {
		return errors.New("breaker event exceeds bounded payload budget")
	}
	var next *time.Time
	if !l.NextProbeAt.IsZero() {
		at := l.NextProbeAt
		next = &at
	}
	row := models.BreakerIncident{ID: l.ID, Site: l.Site, Stage: l.Stage, State: l.State, Revision: l.Revision, Active: l.Active(), NextProbeAt: next, StartedAt: l.StartedAt, UpdatedAt: time.Now(), Payload: datatypes.JSON(payload)}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous models.BreakerIncident
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", row.ID).Take(&previous).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			return appendHistory(tx, snapshot)
		}
		if err != nil {
			return err
		}
		if previous.Site != row.Site || (previous.Stage != row.Stage && row.Stage != "") || previous.Revision > row.Revision {
			return ErrStale
		}
		if previous.Revision == row.Revision {
			// MySQL canonicalizes JSON spacing/key order; compare decoded documents.
			var prior Snapshot
			if err := json.Unmarshal(previous.Payload, &prior); err != nil {
				return fmt.Errorf("decode persisted breaker event: %w", err)
			}
			canonical, err := json.Marshal(prior)
			if err != nil {
				return err
			}
			if bytes.Equal(canonical, payload) {
				return appendHistory(tx, snapshot)
			}
			return ErrStale
		}
		if err := tx.Model(&row).Select("stage", "state", "revision", "active", "next_probe_at", "updated_at", "payload").Updates(&row).Error; err != nil {
			return err
		}
		return appendHistory(tx, snapshot)
	})
}

func decodeSnapshots(rows []models.BreakerIncident) ([]Snapshot, error) {
	out := make([]Snapshot, 0, len(rows))
	for _, row := range rows {
		if len(row.Payload) > maxPayload {
			return nil, fmt.Errorf("breaker event %s exceeds payload budget", row.ID)
		}
		var snapshot Snapshot
		if err := json.Unmarshal(row.Payload, &snapshot); err != nil {
			return nil, fmt.Errorf("decode breaker event %s: %w", row.ID, err)
		}
		if err := snapshot.ValidateCurrent(); err != nil {
			return nil, fmt.Errorf("breaker event %s: %w", row.ID, err)
		}
		l := snapshot.Control
		if l.ID != row.ID || l.Site != row.Site || l.Stage != row.Stage || l.Revision != row.Revision || l.State != row.State || l.Active() != row.Active {
			return nil, fmt.Errorf("breaker event %s metadata mismatch", row.ID)
		}
		out = append(out, snapshot.Clean())
	}
	return out, nil
}

func decode(rows []models.BreakerIncident) ([]incident.Lifecycle, error) {
	snapshots, err := decodeSnapshots(rows)
	if err != nil {
		return nil, err
	}
	out := make([]incident.Lifecycle, 0, len(snapshots))
	for _, snapshot := range snapshots {
		out = append(out, snapshot.Lifecycle)
	}
	return out, nil
}

// LoadRuntime includes the latest closed event per site so force release does
// not discard its failure window on restart. Multiple active rows remain visible
// and are rejected by Engine instead of silently choosing one.
func (s *Store) LoadRuntime(ctx context.Context) ([]Snapshot, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("breaker store is not configured")
	}
	var rows []models.BreakerIncident
	q := s.db.WithContext(ctx).Where("active = ? OR NOT EXISTS (SELECT 1 FROM crawler_breaker_incidents AS newer WHERE newer.site = crawler_breaker_incidents.site AND (newer.started_at > crawler_breaker_incidents.started_at OR (newer.started_at = crawler_breaker_incidents.started_at AND newer.id > crawler_breaker_incidents.id)))", true)
	if err := q.Order("started_at ASC").Order("id ASC").Limit(MaxActive + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > MaxActive {
		return nil, errors.New("breaker runtime sites exceed restore budget")
	}
	return decodeSnapshots(rows)
}

func (s *Store) List(ctx context.Context, site string, limit int) ([]incident.Lifecycle, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("breaker store is not configured")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	var rows []models.BreakerIncident
	query := s.db.WithContext(ctx)
	if site != "" {
		query = query.Where("site = ?", site)
	}
	if err := query.Order("started_at DESC").Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return decode(rows)
}
