package database

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Upsert 执行 INSERT ... ON DUPLICATE KEY UPDATE：冲突时按 conflictColumns 更新 updateColumns，
// 并回填主键，使 record 指向最终记录（含冲突路径查回的 ID）。
func Upsert(db *gorm.DB, record any, conflictColumns, updateColumns []string) error {
	if err := db.Clauses(clause.OnConflict{
		Columns:   columnList(conflictColumns),
		DoUpdates: clause.AssignmentColumns(updateColumns),
	}).Create(record).Error; err != nil {
		return err
	}
	return backfillPrimaryKey(db, record, conflictColumns)
}

func columnList(cols []string) []clause.Column {
	out := make([]clause.Column, len(cols))
	for i, c := range cols {
		out[i] = clause.Column{Name: c}
	}
	return out
}

// backfillPrimaryKey GORM 的 OnConflict 在冲突更新路径不会回填自增主键，
// 此时按 conflictColumns（唯一约束列）查回并回填到 record。
func backfillPrimaryKey(db *gorm.DB, record any, conflictColumns []string) error {
	s, err := schema.Parse(record, &sync.Map{}, db.NamingStrategy)
	if err != nil {
		return err
	}
	pk := s.PrioritizedPrimaryField
	if pk == nil {
		return nil // 无主键，无需回填
	}
	rv := reflect.ValueOf(record)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("database.Upsert: record must be a non-nil pointer")
	}
	if _, zero := pk.ValueOf(context.Background(), rv); !zero {
		return nil // 主键已回填（插入路径）
	}

	query := db
	for _, col := range conflictColumns {
		f := s.FieldsByDBName[col]
		if f == nil {
			return fmt.Errorf("database.Upsert: conflict column %q not found on %s", col, s.Name)
		}
		v, _ := f.ValueOf(context.Background(), rv)
		query = query.Where(clause.Eq{Column: clause.Column{Name: f.DBName}, Value: v})
	}
	return query.First(record).Error
}
