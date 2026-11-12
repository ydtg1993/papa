package gormstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/account/contract"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

// PoolRecord stores membership, current identity and a bounded, credential-free
// switch history. Explicit migration only: constructing a store never runs DDL.
type PoolRecord struct {
	Site      string `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点键"`
	PoolName  string `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点内账号池名"`
	Revision  uint64 `gorm:"not null;comment:池状态CAS版本"`
	StateJSON string `gorm:"type:longtext;not null;comment:成员顺序及当前账号与有界切换记录，不含凭据"`
}

func (PoolRecord) TableName() string { return "crawler_account_pools" }

type ManagementRecord struct {
	Site      string `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点键"`
	AccountID string `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点内账号ID"`
	Revision  uint64 `gorm:"not null;comment:管理状态CAS版本"`
	StateJSON string `gorm:"type:longtext;not null;comment:停用隔离冷却与待处理失效状态，不含凭据"`
}

func (ManagementRecord) TableName() string { return "crawler_account_management" }
func Models() []any                        { return []any{&PoolRecord{}, &ManagementRecord{}} }

type GORMPoolStore struct{ db *gorm.DB }

func duplicatePoolKey(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &mysqlErr) && mysqlErr.Number == 1062)
}

func NewGORMPoolStore(db *gorm.DB) *GORMPoolStore { return &GORMPoolStore{db: db} }
func (s *GORMPoolStore) LoadPools(ctx context.Context) ([]contract.PoolState, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account pool database required")
	}
	var rows []PoolRecord
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]contract.PoolState, 0, len(rows))
	for _, row := range rows {
		var v contract.PoolState
		if err := json.Unmarshal([]byte(row.StateJSON), &v); err != nil {
			return nil, errors.New("invalid stored account pool")
		}
		if v.Key != (contract.PoolKey{Site: row.Site, Name: row.PoolName}) || v.Revision != row.Revision {
			return nil, errors.New("stored account pool identity mismatch")
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *GORMPoolStore) LoadManagement(ctx context.Context) ([]contract.ManagementState, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("account pool database required")
	}
	var rows []ManagementRecord
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]contract.ManagementState, 0, len(rows))
	for _, row := range rows {
		var v contract.ManagementState
		if err := json.Unmarshal([]byte(row.StateJSON), &v); err != nil {
			return nil, errors.New("invalid stored account management state")
		}
		if v.Key != (contract.Key{Site: row.Site, ID: row.AccountID}) || v.Version != row.Revision {
			return nil, errors.New("stored account management identity mismatch")
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *GORMPoolStore) SavePoolCAS(ctx context.Context, v contract.PoolState, expected uint64) error {
	if s == nil || s.db == nil {
		return errors.New("account pool database required")
	}
	if v.Revision <= expected {
		return contract.ErrPoolConflict
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	row := PoolRecord{Site: v.Key.Site, PoolName: v.Key.Name, Revision: v.Revision, StateJSON: string(b)}
	if expected == 0 {
		err = s.db.WithContext(ctx).Create(&row).Error
		if duplicatePoolKey(err) {
			return contract.ErrPoolConflict
		}
		if err != nil {
			var existing PoolRecord
			if s.db.WithContext(ctx).Where("site = ? AND pool_name = ?", row.Site, row.PoolName).First(&existing).Error == nil {
				if existing.Revision == row.Revision && existing.StateJSON == row.StateJSON {
					return nil
				}
				return contract.ErrPoolConflict
			}
		}
		return err
	}
	r := s.db.WithContext(ctx).Model(&PoolRecord{}).Where("site = ? AND pool_name = ? AND revision = ?", row.Site, row.PoolName, expected).Updates(map[string]any{"revision": row.Revision, "state_json": row.StateJSON})
	if r.Error == nil && r.RowsAffected == 0 {
		return contract.ErrPoolConflict
	}
	if r.Error != nil || r.RowsAffected == 0 {
		var existing PoolRecord
		if readErr := s.db.WithContext(ctx).Where("site = ? AND pool_name = ?", row.Site, row.PoolName).First(&existing).Error; readErr == nil {
			if existing.Revision == row.Revision && existing.StateJSON == row.StateJSON {
				return nil
			}
			return contract.ErrPoolConflict
		}
		if r.Error != nil {
			return r.Error
		}
		return contract.ErrPoolConflict
	}
	return nil
}
func (s *GORMPoolStore) SaveManagementCAS(ctx context.Context, v contract.ManagementState, expected uint64) error {
	if s == nil || s.db == nil {
		return errors.New("account pool database required")
	}
	if v.Version <= expected {
		return contract.ErrPoolConflict
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	row := ManagementRecord{Site: v.Key.Site, AccountID: v.Key.ID, Revision: v.Version, StateJSON: string(b)}
	if expected == 0 {
		err = s.db.WithContext(ctx).Create(&row).Error
		if duplicatePoolKey(err) {
			return contract.ErrPoolConflict
		}
		if err != nil {
			var existing ManagementRecord
			if s.db.WithContext(ctx).Where("site = ? AND account_id = ?", row.Site, row.AccountID).First(&existing).Error == nil {
				if existing.Revision == row.Revision && existing.StateJSON == row.StateJSON {
					return nil
				}
				return contract.ErrPoolConflict
			}
		}
		return err
	}
	r := s.db.WithContext(ctx).Model(&ManagementRecord{}).Where("site = ? AND account_id = ? AND revision = ?", row.Site, row.AccountID, expected).Updates(map[string]any{"revision": row.Revision, "state_json": row.StateJSON})
	if r.Error == nil && r.RowsAffected == 0 {
		return contract.ErrPoolConflict
	}
	if r.Error != nil || r.RowsAffected == 0 {
		var existing ManagementRecord
		if readErr := s.db.WithContext(ctx).Where("site = ? AND account_id = ?", row.Site, row.AccountID).First(&existing).Error; readErr == nil {
			if existing.Revision == row.Revision && existing.StateJSON == row.StateJSON {
				return nil
			}
			return contract.ErrPoolConflict
		}
		if r.Error != nil {
			return r.Error
		}
		return contract.ErrPoolConflict
	}
	return nil
}
