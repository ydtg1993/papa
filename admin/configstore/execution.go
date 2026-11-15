package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/models"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"time"
)

// ExecutionStore adapts the shared CAS store; no runtime defaults live here.
type ExecutionStore struct{ Store *Store }

func (s ExecutionStore) Load(ctx context.Context) (ep.History, error) {
	var row models.Config
	err := s.Store.db.WithContext(ctx).Where("cfg_key = ?", ep.Key).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ep.History{}, ep.ErrNotFound
	}
	if err != nil {
		return ep.History{}, err
	}
	if row.Revision == 0 {
		return ep.History{}, fmt.Errorf("invalid execution policy revision")
	}
	d, err := ep.Decode(row.CfgVal)
	return ep.History{Revision: row.Revision, Document: d, Operator: row.UpdatedBy, CreatedAt: row.UpdatedAt}, err
}

func (s ExecutionStore) Save(ctx context.Context, u ep.Update, operator string) (ep.History, error) {
	var h ep.History
	err := s.Store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rev, err := New(tx).saveCAS(ctx, ep.Key, u.Document, u.ExpectedRevision, operator)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(u.Document)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		op := models.ConfigOperation{OperationID: u.OperationID, ConfigKey: ep.Key, ExpectedRevision: u.ExpectedRevision, Revision: rev, Document: datatypes.JSON(raw), Operator: operator, Reason: u.Reason, CreatedAt: now}
		if err := tx.Create(&op).Error; err != nil {
			return err
		}
		if err := tx.Where("cfg_key = ? AND created_at < ?", ep.Key, now.Add(-90*24*time.Hour)).Delete(&models.ConfigOperation{}).Error; err != nil {
			return err
		}
		h = ep.History{Revision: rev, Document: u.Document.Clone(), Operator: operator, Reason: u.Reason, OperationID: u.OperationID, CreatedAt: now}
		return nil
	})
	if errors.Is(err, ErrConflict) {
		err = ep.ErrConflict
	}
	var duplicate *driver.MySQLError
	if errors.As(err, &duplicate) && duplicate.Number == 1062 {
		err = ep.ErrConflict
	}
	return h, err
}
func (s ExecutionStore) History(ctx context.Context, limit int) ([]ep.History, error) {
	rows, err := s.Store.History(ctx, ep.Key)
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > 100 {
		limit = 100
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	revisions := make([]uint64, 0, len(rows))
	for _, r := range rows {
		revisions = append(revisions, r.Revision)
	}
	operations := map[uint64]models.ConfigOperation{}
	if len(revisions) > 0 {
		var ops []models.ConfigOperation
		if err := s.Store.db.WithContext(ctx).Where("cfg_key = ? AND revision IN ?", ep.Key, revisions).Find(&ops).Error; err != nil {
			return nil, err
		}
		for _, op := range ops {
			operations[op.Revision] = op
		}
	}
	out := make([]ep.History, 0, len(rows))
	for _, r := range rows {
		d, err := ep.Decode(r.Document)
		if err != nil {
			return nil, err
		}
		op := operations[r.Revision]
		out = append(out, ep.History{Revision: r.Revision, Document: d, Operator: r.Operator, CreatedAt: r.CreatedAt, Reason: op.Reason, OperationID: op.OperationID})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (s ExecutionStore) Operation(ctx context.Context, id string) (ep.Operation, error) {
	var r models.ConfigOperation
	err := s.Store.db.WithContext(ctx).Where("cfg_key = ? AND BINARY operation_id = ?", ep.Key, id).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ep.Operation{}, ep.ErrNotFound
	}
	if err != nil {
		return ep.Operation{}, err
	}
	d, err := ep.Decode(r.Document)
	return ep.Operation{ID: r.OperationID, Revision: r.Revision, ExpectedRevision: r.ExpectedRevision, Document: d, Operator: r.Operator, Reason: r.Reason, CreatedAt: r.CreatedAt, Status: "saved"}, err
}
