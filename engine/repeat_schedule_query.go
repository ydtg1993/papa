package engine

import (
	"strings"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// Keep the original expression for dates outside the portable UNIX_TIMESTAMP
// range and unknown/NULL statuses. Converting an out-of-range future date can
// yield zero, so simply selecting the earliest DATETIME would change MIN(epoch).
const soonestRepeatSelect = `MIN(CAST(CASE
 WHEN status IN ? THEN UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW()))
 WHEN next_repeat_at > NOW() THEN UNIX_TIMESTAMP(next_repeat_at)
 ELSE NULL END AS SIGNED)) AS epoch`

const repeatTimeLower = "1971-01-01 00:00:00"
const repeatTimeUpper = "2038-01-01 00:00:00"

// soonestRepeatQueryOn returns one non-locking SELECT. All UNION branches use
// the same statement's NOW and InnoDB consistent read view, not independent
// client round trips. Apply stage caps inside each branch, before LIMIT.
// Unlike the due-task query, this scheduling query intentionally has no
// compensation scope: active/future and blocked rows retain their old meaning.
func (e *Engine) soonestRepeatQueryOn(db *gorm.DB) *gorm.DB {
	known := []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing, models.TaskStatusSuccess, models.TaskStatusFailed}
	base := func() *gorm.DB {
		// Clone without dropping caller scopes; they constrain each candidate,
		// not the derived table after its per-branch LIMIT has already run.
		return e.withRepeatLimits(db.Session(&gorm.Session{}).Model(&models.CrawlerTask{}).
			Where("repeatable = ?", models.RepeatableYes))
	}
	var branches []any
	for _, status := range known {
		q := base().Where("status = ?", status).
			Where("next_repeat_at >= ? AND next_repeat_at < ?", repeatTimeLower, repeatTimeUpper)
		if status == models.TaskStatusPending || status == models.TaskStatusProcessing {
			q = q.Where("next_repeat_at > NOW()")
		} else {
			branches = append(branches, base().Where("status = ? AND next_repeat_at IS NULL", status).
				Select("CAST(UNIX_TIMESTAMP(NOW()) AS SIGNED) AS epoch").Limit(1))
		}
		branches = append(branches, q.Select("CAST(UNIX_TIMESTAMP(next_repeat_at) AS SIGNED) AS epoch").Order("next_repeat_at").Limit(1))
	}
	branches = append(branches,
		base().Where("status IN ? AND (next_repeat_at < ? OR next_repeat_at >= ?)", known, repeatTimeLower, repeatTimeUpper).
			Select(soonestRepeatSelect, repeatDueStatuses),
		base().Where("status NOT IN ? OR status IS NULL", known).
			Select(soonestRepeatSelect, repeatDueStatuses))
	parts := make([]string, len(branches))
	for i := range parts {
		parts[i] = "(?)"
	}
	union := db.Session(&gorm.Session{NewDB: true}).Raw(strings.Join(parts, " UNION ALL "), branches...)
	result := db.Session(&gorm.Session{NewDB: true}).Table("(?) AS repeat_candidates", union).Select("MIN(epoch) AS epoch")
	for _, info := range e.stages {
		if info.config.RepeatLimit > 0 {
			// With no eligible row, one unordered scan is cheaper than walking
			// every status/time range with residual stage caps. This guard and
			// the candidates still share one statement/read view; no cache.
			return db.Session(&gorm.Session{NewDB: true}).Table("(SELECT 1) AS repeat_guard").
				Select("CASE WHEN EXISTS (?) THEN (?) ELSE NULL END AS epoch", base().Select("1").Limit(1), result)
		}
	}
	return result
}
