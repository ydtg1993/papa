package taskstore

import (
	"context"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"time"
)

// PendingWindow excludes processing/terminal and deferred compensation. The
// database clock decides due eligibility, including the bounded prefetch window.
func PendingWindow(ctx context.Context, db *gorm.DB, site, stage string, after uint, window time.Duration, limit int) *gorm.DB {
	return CompensationScope(db.WithContext(ctx).Model(&models.CrawlerTask{})).
		Where("site = ? AND stage = ? AND status = ? AND id > ?", site, stage, models.TaskStatusPending, after).
		Where("not_before IS NULL OR not_before <= TIMESTAMPADD(MICROSECOND, ?, UTC_TIMESTAMP(3))", window.Microseconds()).
		Order("id ASC").Limit(limit)
}

func CountPending(ctx context.Context, db *gorm.DB, site, stage string) (int64, error) {
	var n int64
	err := db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("site = ? AND stage = ? AND status = ?", site, stage, models.TaskStatusPending).Count(&n).Error
	return n, err
}

// LoadSchedulingBatch rechecks detached members in one bounded SQL round trip.
func LoadSchedulingBatch(ctx context.Context, db *gorm.DB, ids []int) ([]models.CrawlerTask, error) {
	var rows []models.CrawlerTask
	if len(ids) == 0 {
		return rows, nil
	}
	err := db.WithContext(ctx).Where("id IN ?", ids).Limit(len(ids)).Find(&rows).Error
	return rows, err
}
