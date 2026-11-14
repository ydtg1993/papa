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

// NextPendingDelay uses the owning clocks of durable eligibility columns. It
// ignores ready rows: capacity and policy releases signal their stage directly.
func NextPendingDelay(ctx context.Context, db *gorm.DB, site, stage string) (time.Duration, error) {
	var row struct{ Micros *int64 }
	err := db.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("site = ? AND stage = ? AND status = ?", site, stage, models.TaskStatusPending).
		Where("compensation_mode IN ?", []string{"", "retry_after", "account_ready"}).
		Where("not_before > UTC_TIMESTAMP(3) OR next_compensation_at > NOW()").
		Select("MIN(GREATEST(COALESCE(TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(3), not_before), 0), COALESCE(TIMESTAMPDIFF(MICROSECOND, NOW(), next_compensation_at), 0), 0)) AS micros").Scan(&row).Error
	if err != nil || row.Micros == nil {
		return 0, err
	}
	return max(time.Millisecond, time.Duration(*row.Micros)*time.Microsecond), nil
}
