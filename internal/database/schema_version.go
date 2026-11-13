package database

import (
	"fmt"
	"gorm.io/gorm"
)

// CheckTaskSchema refuses pre-v4 tables before any migration writes.
// Empty databases are accepted; v4 migrations never backfill historical identity.
func CheckTaskSchema(db *gorm.DB) error {
	var rows []struct{ ColumnName string }
	if err := db.Raw("SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?", "crawler_tasks").Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	columns := make(map[string]bool, len(rows))
	for _, r := range rows {
		columns[r.ColumnName] = true
	}
	if err := validateTaskColumns(columns); err != nil {
		return err
	}
	for _, name := range []string{"idx_site_stage_url_hash", "idx_idempotency_hash"} {
		present, err := HasIndexNamed(db, "crawler_tasks", name)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("unsupported task schema: obsolete index %s; use an independent v4 database", name)
		}
	}

	return checkTaskIdentityData(db)
}

func checkTaskIdentityData(db *gorm.DB) error {
	var invalid int64
	if err := db.Table("crawler_tasks").Where("identity_hash IS NULL OR identity_hash = '' OR identity_type IS NULL OR identity_type NOT IN ('id', 'url') OR identity_value IS NULL OR identity_value = ''").Count(&invalid).Error; err != nil {
		return err
	}
	if invalid > 0 {
		return fmt.Errorf("unsupported task data: %d rows lack v4 identity; automatic conversion is not supported", invalid)
	}
	return nil
}

func validateTaskColumns(columns map[string]bool) error {
	for _, name := range []string{"url_hash", "idempotency_key", "idempotency_hash"} {
		if columns[name] {
			return fmt.Errorf("unsupported task schema: obsolete column %s; use an independent v4 database", name)
		}
	}
	for _, name := range []string{"identity_type", "identity_value", "identity_hash"} {
		if !columns[name] {
			return fmt.Errorf("unsupported task schema: v4 requires %s; use an independent v4 database", name)
		}
	}
	return nil
}
