package engine

import (
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// soonestRepeatQueryOn performs one non-locking aggregate in the caller's
// context/transaction. Moving the original CASE eligibility into WHERE keeps
// unknown/NULL statuses eligible only for future dates; terminal NULL dates
// still use this statement's NOW. Keep CAST inside MIN for the int64 result.
//
// Do not replace MIN(epoch) with ORDER BY next_repeat_at LIMIT 1: permissive
// MySQL modes can store invalid dates inside the normal year range, and their
// UNIX_TIMESTAMP value may be zero. The smallest DATETIME need not produce the
// smallest epoch. Conversion stays in MySQL, preserving its time-zone rules.
// This scheduling read intentionally has no compensation scope; only the
// actual repeat submission path excludes blocked tasks.
func (e *Engine) soonestRepeatQueryOn(db *gorm.DB) *gorm.DB {
	return e.withRepeatLimits(db.Model(&models.CrawlerTask{}).
		Where("repeatable = ?", models.RepeatableYes).
		Where("status IN ? OR next_repeat_at > NOW()", repeatDueStatuses).
		Select("MIN(CAST(UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW())) AS SIGNED)) AS epoch"))
}
