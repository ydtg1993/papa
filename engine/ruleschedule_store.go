package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func scheduleDB(e *Engine) (*gorm.DB, error) {
	if e == nil || e.db == nil || e.db.Dialector == nil || e.db.Dialector.Name() != "mysql" || e.db.DryRun || e.db.Error != nil {
		return nil, ErrRuleScheduleInvalid
	}
	if _, ok := e.db.ConnPool.(gorm.TxCommitter); ok {
		return nil, ErrRuleScheduleInvalid
	}
	if e.db.Statement != nil {
		if _, ok := e.db.Statement.ConnPool.(gorm.TxCommitter); ok {
			return nil, ErrRuleScheduleInvalid
		}
	}
	return e.db.Session(&gorm.Session{NewDB: true, Initialized: true, Context: context.Background()}), nil
}

func (s *RuleSchedule) readSchedule(ctx context.Context, db *gorm.DB, id uint) (scheduleControl, *scheduleState, error) {
	var row models.CrawlerTask
	if err := db.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
		return scheduleControl{}, nil, err
	}
	c, err := s.decodeControl(row)
	if err != nil {
		return c, nil, err
	}
	var stored models.RuleSchedule
	err = db.WithContext(ctx).Where("task_id = ?", id).Take(&stored).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c, nil, nil
	}
	if err != nil {
		return c, nil, err
	}
	state, err := s.decodeState(c, stored)
	return c, state, err
}

func sameScheduleControl(a, b scheduleControl) bool {
	return a.ID == b.ID && a.Site == b.Site && a.Stage == b.Stage && a.URL == b.URL && a.specTask.Meta[scheduleMeta] == b.specTask.Meta[scheduleMeta] && a.Status == b.Status && a.Repeat == b.Repeat && a.Reprocess == b.Reprocess && scheduleTick(a.LastRepeatAt) == scheduleTick(b.LastRepeatAt)
}

// commitBatch never invokes Catalog or task submission while holding row locks.
// A returned commit error is deliberately not converted to dispatch authority.
func (s *RuleSchedule) commitBatch(ctx context.Context, db *gorm.DB, c scheduleControl, old *scheduleState, next schedulePayload) error {
	payload, err := s.encodePayload(c, next)
	if err != nil {
		return err
	}
	revision := uint64(1)
	if old != nil {
		if old.row.Revision == math.MaxUint64 {
			return ErrRuleScheduleCorrupt
		}
		revision = old.row.Revision + 1
	}
	if (old == nil && next.Sequence != 1) || (old != nil && next.Sequence != old.payload.Sequence+1) {
		return ErrRuleScheduleConflict
	}
	row := models.RuleSchedule{TaskID: c.ID, Revision: revision, PayloadJSON: string(payload), PayloadSHA256: ruleDigest(payload)}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked models.CrawlerTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", c.ID).Take(&locked).Error; err != nil {
			return err
		}
		current, err := s.decodeControl(locked)
		if err != nil {
			return err
		}
		if !current.processing() || !sameScheduleControl(c, current) {
			return ErrRuleScheduleConflict
		}
		var stored models.RuleSchedule
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("task_id = ?", c.ID).Take(&stored).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if old != nil {
				return ErrRuleScheduleConflict
			}
			return tx.Create(&row).Error
		}
		if err != nil {
			return err
		}
		if _, err = s.decodeState(current, stored); err != nil {
			return err
		}
		if old == nil || stored.Revision != old.row.Revision || stored.PayloadSHA256 != old.row.PayloadSHA256 || stored.PayloadJSON != old.row.PayloadJSON {
			return ErrRuleScheduleConflict
		}
		result := scheduleStateScope(tx, c.ID, old.row.Revision).Updates(map[string]any{"revision": row.Revision, "payload_json": row.PayloadJSON, "payload_sha256": row.PayloadSHA256})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRuleScheduleConflict
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrRuleScheduleConflict) || errors.Is(err, ErrRuleScheduleCorrupt) {
			return err
		}
		return errors.Join(ErrRuleScheduleUnconfirmed, err)
	}
	return nil
}

func scheduleStateScope(db *gorm.DB, id uint, revision uint64) *gorm.DB {
	return db.Model(&models.RuleSchedule{}).Where("task_id = ? AND revision = ?", id, revision)
}

func scheduleBatchScope(db *gorm.DB, site, batch string, lowerID uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("site = ? AND id >= ? AND JSON_UNQUOTE(JSON_EXTRACT(meta, '$._rule_batch')) = ?", site, lowerID, batch)
}

func (s *RuleSchedule) batchStatus(ctx context.Context, db *gorm.DB, c scheduleControl, state *scheduleState) (RuleScheduleStatus, error) {
	out := RuleScheduleStatus{TaskID: c.ID}
	if state == nil {
		return out, nil
	}
	p := state.payload
	out.Revision, out.Sequence, out.BatchID = state.row.Revision, p.Sequence, p.BatchID
	release, err := decodeRuleRelease(p.Root.Meta[ruleReleaseMeta])
	if err != nil {
		return out, ErrRuleScheduleCorrupt
	}
	out.ReleaseID, out.ReleaseGeneration = release.ID, release.Generation
	var root models.CrawlerTask
	err = db.WithContext(ctx).Where("site = ? AND stage = ? AND url_hash = ?", p.Root.Site, p.Root.Stage, models.UrlHash(p.Root.URL)).Take(&root).Error
	lower := c.ID
	if err == nil {
		var meta map[string]string
		if root.ID <= c.ID || root.URL != p.Root.URL || root.IdempotencyKey != "" || root.Repeatable != models.RepeatableNo || json.Unmarshal(root.Meta, &meta) != nil || ruleJSON(meta) != ruleJSON(p.Root.Meta) {
			return out, ErrRuleScheduleCorrupt
		}
		out.RootID, lower = root.ID, root.ID
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return out, err
	}
	var groups []struct {
		Status models.TaskStatus
		Count  int64
	}
	if err := scheduleBatchScope(db.WithContext(ctx), c.Site, p.BatchID, lower).Select("status, COUNT(*) AS count").Group("status").Scan(&groups).Error; err != nil {
		return out, err
	}
	for _, g := range groups {
		if g.Count < 0 || out.Total > math.MaxInt64-g.Count {
			return out, ErrRuleScheduleCorrupt
		}
		out.Total += g.Count
		switch g.Status {
		case models.TaskStatusPending:
			out.Pending = g.Count
		case models.TaskStatusProcessing:
			out.Processing = g.Count
		case models.TaskStatusSuccess:
			out.Success = g.Count
		case models.TaskStatusFailed:
			out.Failed = g.Count
		default:
			return out, ErrRuleScheduleCorrupt
		}
	}
	if (out.RootID == 0 && out.Total != 0) || (out.RootID != 0 && out.Total == 0) {
		return out, ErrRuleScheduleCorrupt
	}
	out.Complete = out.RootID != 0 && out.Total > 0 && out.Total == out.Success
	return out, nil
}
