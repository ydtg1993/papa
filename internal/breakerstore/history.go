package breakerstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func appendHistory(tx *gorm.DB, snapshot Snapshot) error {
	rows := make([]models.BreakerHistory, 0, len(snapshot.History))
	for _, entry := range snapshot.History {
		if entry.Sequence == 0 || entry.Sequence > snapshot.Control.HistorySequence {
			return fmt.Errorf("invalid breaker history sequence")
		}
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		rows = append(rows, models.BreakerHistory{IncidentID: snapshot.Control.ID, Sequence: entry.Sequence, Site: snapshot.Control.Site, At: entry.At, State: entry.To, Payload: datatypes.JSON(payload)})
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 64).Error
}

// HistoryForWindow returns the state entering the requested day, plus that
// day's append-only transitions. It never joins the mutable latest snapshot.
func (s *Store) HistoryForWindow(ctx context.Context, from, to time.Time, limit int) ([]models.BreakerHistory, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("breaker store is not configured")
	}
	if limit <= 0 || limit > 10000 {
		limit = 10000
	}
	q := s.db.WithContext(ctx).Model(&models.BreakerHistory{})
	if !to.IsZero() {
		q = q.Where("at < ?", to.UTC().Format("2006-01-02 15:04:05.000000"))
	}
	if !from.IsZero() {
		cutoff := from.UTC().Format("2006-01-02 15:04:05.000000")
		q = q.Where("at >= ? OR (state NOT IN ? AND NOT EXISTS (SELECT 1 FROM crawler_breaker_history AS newer WHERE newer.incident_id = crawler_breaker_history.incident_id AND newer.at < ? AND newer.sequence > crawler_breaker_history.sequence))", cutoff, []string{"closed", "recovered"}, cutoff)
	}
	var rows []models.BreakerHistory
	if err := q.Order("at ASC").Order("incident_id ASC").Order("sequence ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	limited := len(rows) > limit
	if limited {
		rows = rows[:limit]
	}
	return rows, limited, nil
}

func HistoryText(row models.BreakerHistory) (string, error) {
	var entry incident.Transition
	if err := json.Unmarshal(row.Payload, &entry); err != nil {
		return "", err
	}
	entry = (incident.Lifecycle{Timeline: []incident.Transition{entry}}).Clean().Timeline[0]
	text := fmt.Sprintf("%s；周期 %d / 轮次 %d；%s；%s → %s；阶段 %s；失败/成功 %d/%d；%s", entry.At.Format(time.RFC3339), entry.Cycle, entry.Round, entry.Action, entry.From, entry.To, entry.Stage, entry.Failures, entry.Successes, entry.Reason)
	if c := entry.Check; c != nil {
		text += fmt.Sprintf("；验证方式=%s；当时覆盖=%s；样本=%v；%s", c.Verification, c.Coverage, c.SampleIDs, c.VerificationNote)
	}
	if !entry.NextAttemptAt.IsZero() {
		text += "；下次计划=" + entry.NextAttemptAt.Format(time.RFC3339)
	}
	return text, nil
}
