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
	return ClaimScope(db, id).Where("flow_id = ?", "").Updates(ClaimUpdates())
}
func Release(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ?", id, models.TaskStatusProcessing).Updates(map[string]any{"status": models.TaskStatusPending})
}
func Insert(db *gorm.DB, row *models.CrawlerTask) error { return db.Create(row).Error }

// IncrementRetry reports persistence failure to the execution owner.
func IncrementRetry(db *gorm.DB, id int) error {
	return db.Model(&models.CrawlerTask{}).Where("id = ?", id).Update("retry", gorm.Expr("retry + ?", 1)).Error
}

// retry advances the monotonic generation, while only automatic retries consume budget.
// The caller supplies a predicate built with RetryScope or CompensationScope.
func retry(scope *gorm.DB, automatic bool) *gorm.DB {
	updates := map[string]any{"status": models.TaskStatusPending, "retry": 0, "reprocess": gorm.Expr("reprocess + 1"), "compensation_version": gorm.Expr("compensation_version + 1")}
	if automatic {
		updates["auto_retry_count"] = gorm.Expr("auto_retry_count + 1")
	} else {
		updates["retry_blocked"] = gorm.Expr("COALESCE(retry_blocked, status = ?)", models.TaskStatusFailed)
	}
	return scope.Updates(updates)
}
func advanceRepeat(scope *gorm.DB, intervalSeconds int64) *gorm.DB {
	return scope.Updates(map[string]any{
		"status": models.TaskStatusPending, "compensation_version": gorm.Expr("compensation_version + 1"), "repeat": gorm.Expr("`repeat` + 1"), "last_repeat_at": gorm.Expr("NOW()"),
		"next_repeat_at": gorm.Expr("FROM_UNIXTIME(UNIX_TIMESTAMP(NOW()) + COALESCE(NULLIF(repeat_interval, 0), ?))", intervalSeconds),
	})
}

// RetryTask fences an explicit operator request against its observed round.
func RetryTask(db *gorm.DB, id uint, status models.TaskStatus, version RunVersion, compensationVersion uint64) *gorm.DB {
	return retry(RetryScope(db, id, version.Reprocess).Where("status = ? AND `repeat` = ? AND compensation_version = ?", status, version.Repeat, compensationVersion), false)
}

// RetryFailed claims one eligible error-queue record without resetting a row
// already claimed by execution or polling. maxRetry <= 0 means unlimited.
func RetryFailed(db *gorm.DB, id uint, version RunVersion, compensationVersion uint64, maxRetry int) *gorm.DB {
	scope := CompensationScope(db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ? AND retry_blocked = ?", id, models.TaskStatusFailed, false)).Where("compensation_version = ? AND `repeat` = ? AND reprocess = ?", compensationVersion, version.Repeat, version.Reprocess)
	if maxRetry > 0 {
		scope = scope.Where("auto_retry_count < ?", maxRetry)
	}
	return retry(scope, true)
}
func RepeatScope(db *gorm.DB, id uint) *gorm.DB {
	return CompensationScope(db.Model(&models.CrawlerTask{}).Where("id = ? AND repeatable = ? AND status = ?", id, models.RepeatableYes, models.TaskStatusSuccess))
}
func AdvanceRepeat(db *gorm.DB, id uint, version RunVersion, compensationVersion uint64, limit int, intervalSeconds int64, force bool) *gorm.DB {
	scope := RepeatScope(db, id).Where("compensation_version = ? AND `repeat` = ? AND reprocess = ?", compensationVersion, version.Repeat, version.Reprocess)
	if !force {
		scope = scope.Where("next_repeat_at IS NULL OR next_repeat_at <= NOW()")
	}
	if limit > 0 {
		scope = scope.Where("`repeat` < ?", limit)
	}
	return advanceRepeat(scope, intervalSeconds)
}

// ScheduleFirstRepeat initializes the first deadline in the terminal CAS itself.
// Later terminal writes preserve the existing schedule, including acknowledgements
// retried after an ambiguous commit.
func ScheduleFirstRepeat(updates map[string]any, intervalSeconds float64) {
	updates["next_repeat_at"] = gorm.Expr("CASE WHEN repeatable = ? AND next_repeat_at IS NULL THEN FROM_UNIXTIME(UNIX_TIMESTAMP(NOW(3)) + COALESCE(NULLIF(repeat_interval, 0), ?)) ELSE next_repeat_at END", models.RepeatableYes, intervalSeconds)
}

// ClaimUpdates materializes legacy pending policy and initializes the first
// repeat generation in the same write that owns execution.
func ClaimUpdates() map[string]any {
	return map[string]any{"status": models.TaskStatusProcessing, "urgent": false, "retry_blocked": gorm.Expr("COALESCE(retry_blocked, FALSE)"), "repeat": gorm.Expr("CASE WHEN repeatable = ? AND `repeat` = 0 THEN 1 ELSE `repeat` END", models.RepeatableYes)}
}
