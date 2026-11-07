package agentstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

type RunFilter struct {
	Site, Status, Before string
	From, Until          time.Time
}

func (f RunFilter) Validate() error {
	if len(f.Site) > 128 || !utf8.ValidString(f.Site) || strings.TrimSpace(f.Site) != f.Site || strings.ContainsAny(f.Site, "\x00\r\n") || f.Status != "" && f.Status != Running && !terminal(f.Status) || f.Before != "" && !identifier(f.Before, 128) || f.From.IsZero() || !f.From.Before(f.Until) || f.Until.Sub(f.From) > 7*24*time.Hour {
		return ErrInvalid
	}
	return nil
}

type RunPage struct {
	Items   []Run
	Scanned int
	Next    string
}
type RunDetail struct {
	Run   Run
	Steps []Step
	Next  uint64
}

func runListQuery(db *gorm.DB, f RunFilter) *gorm.DB {
	q := db.Where("created_at >= ? AND created_at < ?", f.From.UTC(), f.Until.UTC())
	if f.Before != "" {
		q = q.Where("id < ?", f.Before)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	return q.Order("id DESC").Limit(51)
}

// List reads at most 50 candidates (plus lookahead), not a global site total.
// Cross-page results are live; IDs are opaque binary keys, not timestamps.
func (s *MySQLStore) List(ctx context.Context, f RunFilter) (RunPage, error) {
	out := RunPage{Items: []Run{}}
	if ctx == nil {
		return out, ErrInvalid
	}
	if err := f.Validate(); err != nil {
		return out, err
	}
	var rows []models.AgentRun
	if err := runListQuery(s.db.WithContext(ctx), f).Find(&rows).Error; err != nil {
		return out, dbError(err)
	}
	if len(rows) > 50 {
		rows = rows[:50]
		out.Next = rows[49].ID
	}
	out.Scanned = len(rows)
	for _, row := range rows {
		r := publicRun(row)
		if err := validateStoredRun(r); err != nil {
			return out, err
		}
		var snapshot struct{ Goal struct{ Site string } }
		if json.Unmarshal([]byte(r.SnapshotJSON), &snapshot) != nil {
			return out, ErrCorrupt
		}
		if f.Site == "" || f.Site == snapshot.Goal.Site {
			out.Items = append(out.Items, r)
		}
	}
	return out, nil
}
func stepPageQuery(db *gorm.DB, id string, after, cutoff uint64) *gorm.DB {
	return db.Where("run_id = ? AND sequence > ? AND sequence <= ?", id, after, cutoff).Order("sequence ASC").Limit(101)
}

// Inspect freezes run status/revision and one step page in one read snapshot.
func (s *MySQLStore) Inspect(ctx context.Context, id string, after uint64) (RunDetail, error) {
	out := RunDetail{Steps: []Step{}}
	if ctx == nil {
		return out, ErrInvalid
	}
	if err := readKey(ctx, id); err != nil {
		return out, err
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.AgentRun
		if err := tx.Where("id = ?", id).Take(&row).Error; err != nil {
			return dbError(err)
		}
		out.Run = publicRun(row)
		if err := validateStoredRun(out.Run); err != nil {
			return err
		}
		if row.PendingSequence > row.NextSequence {
			return ErrCorrupt
		}
		var steps []models.AgentStep
		if err := stepPageQuery(tx, id, after, row.NextSequence).Find(&steps).Error; err != nil {
			return err
		}
		if len(steps) > 100 {
			steps = steps[:100]
			out.Next = steps[99].Sequence
		}
		expected := after
		for _, p := range steps {
			step := publicStep(p)
			if validateStoredStep(step) != nil || expected == ^uint64(0) || step.Sequence != expected+1 {
				return ErrCorrupt
			}
			expected = step.Sequence
			out.Steps = append(out.Steps, step)
		}
		if out.Next == 0 && expected < row.NextSequence {
			return ErrCorrupt
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return out, dbError(err)
}
