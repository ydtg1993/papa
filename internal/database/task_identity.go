package database

import (
	"fmt"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// migrateTaskIdentities runs only through explicit Migrate, with workers stopped.
// The new nullable unique index is installed before old indexes are removed.
// Conflicts abort without deleting or merging any task; reruns resume backfill.
func migrateTaskIdentities(db *gorm.DB) error {
	var cursor uint
	for {
		var rows []models.CrawlerTask
		if err := db.Where("id > ?", cursor).Order("id").Limit(256).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			cursor = row.ID
			if row.IdentityHash != "" {
				continue
			}
			identity, err := core.NormalizeTaskIdentity(row.Site, row.Stage, row.IdempotencyKey, row.URL)
			if err != nil {
				return fmt.Errorf("task %d identity migration: %w", row.ID, err)
			}
			result := db.Model(&models.CrawlerTask{}).Where("id = ? AND (identity_hash IS NULL OR identity_hash = '')", row.ID).
				Updates(map[string]any{"identity_type": identity.Type, "identity_value": identity.Value, "identity_hash": identity.Hash})
			if result.Error != nil {
				return fmt.Errorf("task %d identity migration (resolve conflicting identities explicitly; no tasks were merged): %w", row.ID, result.Error)
			}
		}
		if len(rows) < 256 {
			break
		}
	}
	for _, name := range []string{"idx_site_stage_url_hash", "idx_idempotency_hash"} {
		exists, err := HasIndexNamed(db, "crawler_tasks", name)
		if err != nil {
			return err
		}
		if exists {
			if err := db.Migrator().DropIndex(&models.CrawlerTask{}, name); err != nil {
				return err
			}
		}
	}
	return nil
}
