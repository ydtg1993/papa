// Package taskstore owns semantic task persistence queries. Scheduling,
// cancellation policy, retry timing and in-memory ownership belong to callers.
package taskstore

import (
	"context"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// PendingBatch reads a bounded keyset page for exactly one site and stage.
// It deliberately includes future not_before rows so the scheduler can register
// their deadlines without losing them when its ID cursor advances.
func PendingBatch(db *gorm.DB, ctx context.Context, site, stage string, cursor uint, limit int) ([]models.CrawlerTask, error) {
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	var rows []models.CrawlerTask
	err := db.WithContext(ctx).Where("site = ? AND stage = ? AND status = ? AND id > ?",
		site, stage, models.TaskStatusPending, cursor).Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

// ResetOrphans is startup-only: the caller must invoke it before any admission
// or claim in this single-instance process. It must never reset live workers.
func ResetOrphans(db *gorm.DB, ctx context.Context) error {
	return db.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("status = ?", models.TaskStatusProcessing).
		Updates(map[string]any{"status": models.TaskStatusPending}).Error
}

// Settle transitions processing to a terminal status. The processing predicate
// makes a retry after an ambiguous commit idempotent, including error append.
// Zero affected rows means the task was already settled/deleted by another
// authorized operation; it is not a reason to overwrite that operation.
func Settle(db *gorm.DB, ctx context.Context, id uint, status models.TaskStatus, cause error, repeatInterval time.Duration) error {
	if status != models.TaskStatusSuccess && status != models.TaskStatusFailed {
		return fmt.Errorf("task settlement requires a terminal status, got %d", status)
	}
	updates := map[string]any{
		"status": status,
		// Initialize only unplanned repeatable tasks; never move an existing deadline.
		"next_repeat_at": gorm.Expr("CASE WHEN repeatable = ? AND next_repeat_at IS NULL THEN FROM_UNIXTIME(UNIX_TIMESTAMP(NOW(3)) + COALESCE(NULLIF(repeat_interval, 0), ?)) ELSE next_repeat_at END", models.RepeatableYes, repeatInterval.Seconds()),
	}
	if status == models.TaskStatusFailed {
		msg := ""
		if cause != nil {
			msg = cause.Error()
		}
		updates["error"] = gorm.Expr("CONCAT(COALESCE(error, ''), ?)", msg+"\n")
	}
	return db.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ?", id, models.TaskStatusProcessing).Updates(updates).Error
}

// ClaimScope admits only pending tasks whose persisted delay has elapsed. Both
// state and deadline are tested in the UPDATE itself, not from an old snapshot.
func ClaimScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ?", id, models.TaskStatusPending).
		Where("not_before IS NULL OR not_before <= ?", time.Now())
}

// GenerationScope rejects a selection from before either governance queue or an
// administrative retry opened another round. Status alone cannot detect ABA.
func GenerationScope(db *gorm.DB, row *models.CrawlerTask) *gorm.DB {
	return db.Where("id = ? AND `repeat` = ? AND reprocess = ?", row.ID, row.Repeat, row.Reprocess)
}
