package configstore

import (
	"context"
	"errors"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

const RuntimeHistoryLimit = 100

// History lists recent committed revisions, without changing policy or retention.
func (s *Store) History(ctx context.Context, key string) ([]models.RuntimePolicyHistory, error) {
	if key != KeyRuntimePolicy {
		return nil, ErrCASRequired
	}
	rows := make([]models.RuntimePolicyHistory, 0)
	err := s.db.WithContext(ctx).Where("cfg_key = ?", key).Order("revision DESC").Limit(RuntimeHistoryLimit).Find(&rows).Error
	return rows, err
}

func (s *Store) HistoryRevision(ctx context.Context, key string, revision uint64) (models.RuntimePolicyHistory, error) {
	var row models.RuntimePolicyHistory
	if key != KeyRuntimePolicy {
		return row, ErrCASRequired
	}
	err := s.db.WithContext(ctx).Where("cfg_key = ? AND revision = ?", key, revision).Take(&row).Error
	if err == nil && (row.ConfigKey != key || row.Revision != revision || len(row.Document) == 0) {
		err = errors.New("历史策略内容或版本无效")
	}
	return row, err
}

// archiveRuntimePrevious preserves a pre-upgrade revision on its first update.
// A duplicate primary key retains the already archived immutable document.
func archiveRuntimePrevious(tx *gorm.DB, key string, expected uint64) error {
	if expected == 0 {
		return nil
	}
	return tx.Exec("INSERT INTO crawler_runtime_policy_history (cfg_key, revision, document, operator, created_at) SELECT cfg_key, revision, cfg_val, updated_by, updated_at FROM crawler_config WHERE cfg_key = ? AND revision = ? ON DUPLICATE KEY UPDATE revision = crawler_runtime_policy_history.revision", key, expected).Error
}
