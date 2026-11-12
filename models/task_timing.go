package models

import (
	"fmt"
	"gorm.io/gorm"
	"math"
	"time"
)

// BeforeSave resolves relative scheduling before INSERT using the database clock.
// Updates never reapply a relative delay. A failed clock read prevents insertion.
func (t *CrawlerTask) BeforeSave(tx *gorm.DB) error {
	if t.ID != 0 {
		return nil
	}
	if t.DelayMS < 0 || t.DelayMS > math.MaxInt64/1000 {
		return fmt.Errorf("task delay milliseconds outside supported range")
	}
	if t.NotBefore != nil {
		at := t.NotBefore.UTC()
		if remainder := at.Nanosecond() % int(time.Millisecond); remainder != 0 {
			at = at.Add(time.Millisecond - time.Duration(remainder))
		}
		t.NotBefore = &at
	}
	if t.NotBefore != nil || t.DelayMS == 0 {
		return nil
	}
	if tx.DryRun {
		return nil
	}
	var at time.Time
	if err := tx.Raw("SELECT DATE_ADD(UTC_TIMESTAMP(3), INTERVAL ? MICROSECOND)", t.DelayMS*1000).Scan(&at).Error; err != nil {
		return fmt.Errorf("resolve task delay: %w", err)
	}
	at = utcWallTime(at)
	t.NotBefore = &at
	return nil
}
