package engine

import (
	"testing"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/database"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func TestTaskIdentityMySQLDistinctIDsSameURLAndRestart(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	a := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/resource", ResourceID: "a"}
	b := &Task{Site: a.Site, Stage: a.Stage, URL: a.URL, ResourceID: "b"}
	if err := e.SubmitTasks([]*Task{a, b}); err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatal("different IDs sharing URL merged")
	}
	e2 := makeEngine()
	c := &Task{Site: a.Site, Stage: a.Stage, URL: "https://example.invalid/moved", ResourceID: "a"}
	if err := e2.SubmitTask(c); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&models.CrawlerTask{}).Count(&count)
	if count != 2 {
		t.Fatalf("restart dedup rows=%d", count)
	}
	var stored models.CrawlerTask
	if err := db.First(&stored, a.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.URL != a.URL || stored.IdentityType != "id" || stored.IdentityValue != "a" {
		t.Fatal("identity or URL overwritten", stored)
	}
	// Repeatable and batch hits must also execute the stored URL, never a URL
	// supplied by a later duplicate submission using the same resource ID.
	for _, batch := range []bool{false, true} {
		duplicate := &Task{Site: a.Site, Stage: a.Stage, URL: "https://example.invalid/replacement", ResourceID: "a", Repeatable: true}
		var err error
		if batch {
			err = e2.SubmitTasks([]*Task{duplicate})
		} else {
			err = e2.SubmitTask(duplicate)
		}
		if err != nil {
			t.Fatal(err)
		}
		if duplicate.URL != a.URL || duplicate.ID != a.ID {
			t.Fatal("duplicate execution URL changed", batch, duplicate)
		}
	}
}

func TestTaskIdentityMySQLExplicitMigration(t *testing.T) {
	db, _ := compensationMySQL(t)
	for _, sql := range []string{"CREATE UNIQUE INDEX idx_site_stage_url_hash ON crawler_tasks(url_hash, stage, site)", "CREATE UNIQUE INDEX idx_idempotency_hash ON crawler_tasks(idempotency_hash)"} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	old := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: "https://EXAMPLE.invalid:443/a#x", URLHash: models.UrlHash("old")}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	// Reproduce the actual old schema with no identity columns, not just empty values.
	if err := db.Migrator().DropIndex(&models.CrawlerTask{}, "idx_task_identity"); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"identity_type", "identity_value", "identity_hash"} {
		if err := db.Migrator().DropColumn(&models.CrawlerTask{}, column); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := database.Migrate(db, &config.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	var got models.CrawlerTask
	if err := db.First(&got, old.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.IdentityValue != "https://example.invalid/a" || len(got.IdentityHash) != 64 {
		t.Fatal("not backfilled", got.IdentityValue)
	}
	for _, index := range []string{"idx_site_stage_url_hash", "idx_idempotency_hash"} {
		if db.Migrator().HasIndex(&models.CrawlerTask{}, index) {
			t.Fatal("obsolete index remains", index)
		}
	}
}

func TestTaskIdentityMySQLMigrationRejectsCanonicalConflict(t *testing.T) {
	db, _ := compensationMySQL(t)
	if err := db.Exec("CREATE UNIQUE INDEX idx_site_stage_url_hash ON crawler_tasks(url_hash, stage, site)").Error; err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://example.invalid/a", "https://example.invalid:443/a#x"} {
		row := models.CrawlerTask{Site: "synthetic", Stage: "detail", URL: u, URLHash: models.UrlHash(u)}
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Migrate(db, &config.Config{}); err == nil {
		t.Fatal("canonical conflict silently merged")
	}
	var count int64
	db.Model(&models.CrawlerTask{}).Count(&count)
	if count != 2 || !db.Migrator().HasIndex(&models.CrawlerTask{}, "idx_site_stage_url_hash") {
		t.Fatal("migration removed rows or old protection before completion")
	}
}
