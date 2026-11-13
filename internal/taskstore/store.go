package taskstore

import (
	"errors"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

var ErrChanged = errors.New("该行已被他人修改，请刷新后重试")

type RunVersion struct {
	Repeat    int
	Reprocess int
}

func CompensationScope(db *gorm.DB) *gorm.DB {
	return db.Where("compensation_mode IN ?", []string{"", string(core.CompensationRetryAfter), string(core.CompensationAccountReady)}).
		Where("compensation_mode <> ? OR account_pool <> ?", string(core.CompensationAccountReady), "").
		Where("next_compensation_at IS NULL OR next_compensation_at <= NOW()")
}
func ClaimScope(db *gorm.DB, id uint) *gorm.DB {
	return CompensationScope(db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ?", id, models.TaskStatusPending).Where("(not_before IS NULL OR not_before <= UTC_TIMESTAMP(3))"))
}
func RetryScope(db *gorm.DB, id uint, reprocess int) *gorm.DB {
	return CompensationScope(db.Model(&models.CrawlerTask{}).Where("id = ? AND status <> ? AND reprocess = ?", id, models.TaskStatusProcessing, reprocess))
}
func TerminalScope(db *gorm.DB, id int, version RunVersion) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ? AND `repeat` = ? AND reprocess = ?", id, models.TaskStatusProcessing, version.Repeat, version.Reprocess)
}

// Complete confirms a lost UPDATE acknowledgement without repeating the append
// or overwriting a newer generation. Updates contains the already frozen outcome.
func Complete(db *gorm.DB, id int, version RunVersion, status models.TaskStatus, updates map[string]any) error {
	result := TerminalScope(db, id, version).Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var current models.CrawlerTask
	if err := db.Select([]string{"status", "repeat", "reprocess"}).Where("id = ?", id).Take(&current).Error; err != nil {
		return err
	}
	if current.Status == status && current.Repeat == version.Repeat && current.Reprocess == version.Reprocess {
		return nil
	}
	return ErrChanged
}
func Claim(db *gorm.DB, id uint) *gorm.DB {
	return ClaimScope(db, id).Where("flow_id = ?", "").Updates(map[string]any{"status": models.TaskStatusProcessing, "urgent": false})
}
func Release(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ?", id, models.TaskStatusProcessing).Updates(map[string]any{"status": models.TaskStatusPending})
}
func Insert(db *gorm.DB, row *models.CrawlerTask) error { return db.Create(row).Error }

// IncrementRetry reports persistence failure to the execution owner.
func IncrementRetry(db *gorm.DB, id int) error {
	return db.Model(&models.CrawlerTask{}).Where("id = ?", id).Update("retry", gorm.Expr("retry + ?", 1)).Error
}

// Retry applies the same generation transition to manual and governance retries.
// The caller supplies a predicate built with RetryScope or CompensationScope.
func retry(scope *gorm.DB) *gorm.DB {
	return scope.Updates(map[string]any{"status": models.TaskStatusPending, "retry": 0, "reprocess": gorm.Expr("reprocess + 1"), "compensation_version": gorm.Expr("compensation_version + 1")})
}
func advanceRepeat(scope *gorm.DB, intervalSeconds int64) *gorm.DB {
	return scope.Updates(map[string]any{
		"status": models.TaskStatusPending, "compensation_version": gorm.Expr("compensation_version + 1"), "repeat": gorm.Expr("`repeat` + 1"), "last_repeat_at": gorm.Expr("NOW()"),
		"next_repeat_at": gorm.Expr("FROM_UNIXTIME(UNIX_TIMESTAMP(NOW()) + COALESCE(NULLIF(repeat_interval, 0), ?))", intervalSeconds),
	})
}

// RetryTask fences an explicit operator request against its observed round.
func RetryTask(db *gorm.DB, id uint, status models.TaskStatus, version RunVersion, compensationVersion uint64) *gorm.DB {
	return retry(RetryScope(db, id, version.Reprocess).Where("status = ? AND `repeat` = ? AND compensation_version = ?", status, version.Repeat, compensationVersion))
}

// RetryFailed claims one eligible error-queue record without resetting a row
// already claimed by execution or polling. maxRetry <= 0 means unlimited.
func RetryFailed(db *gorm.DB, id uint, compensationVersion uint64, maxRetry int) *gorm.DB {
	scope := CompensationScope(db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ?", id, models.TaskStatusFailed)).Where("compensation_version = ?", compensationVersion)
	if maxRetry > 0 {
		scope = scope.Where("reprocess < ?", maxRetry)
	}
	return retry(scope)
}
func RepeatScope(db *gorm.DB, id uint) *gorm.DB {
	return CompensationScope(db.Model(&models.CrawlerTask{}).Where("id = ? AND repeatable = ? AND status IN ?", id, models.RepeatableYes, []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}))
}
func AdvanceRepeat(db *gorm.DB, id uint, compensationVersion uint64, limit int, intervalSeconds int64) *gorm.DB {
	scope := RepeatScope(db, id).Where("compensation_version = ?", compensationVersion)
	if limit > 0 {
		scope = scope.Where("`repeat` < ?", limit)
	}
	return advanceRepeat(scope, intervalSeconds)
}
