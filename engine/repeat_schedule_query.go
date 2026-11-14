package engine

import (
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// soonestRepeatQueryOn selects only successful repeatable rows. Failed and
// active rows cannot wake periodic polling; terminal success wakes the ticker.
// CAST inside MIN preserves MySQL epoch conversion for invalid legacy dates.
// Compensation remains checked by the submission query and conditional write.
func (e *Engine) soonestRepeatQueryOn(db *gorm.DB) *gorm.DB {
	return e.withRepeatLimits(db.Model(&models.CrawlerTask{}).
		Where("repeatable = ?", models.RepeatableYes).
		Where("status = ?", models.TaskStatusSuccess).
		Select("MIN(CAST(UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW())) AS SIGNED)) AS epoch"))
}
