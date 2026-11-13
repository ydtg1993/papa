package engine

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/database"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/datatypes"
	"reflect"
	"testing"
	"time"
)

// Uses the existing explicitly opted-in random database helper; the DSN's
// database is never used. Exercises the production explicit migration entry.
func TestTraceMySQLLegacyMigrationAndLatestTail(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	var environment struct{ Version, DatabaseName string }
	if err := db.Raw("SELECT VERSION() AS version, DATABASE() AS database_name").Scan(&environment).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("MySQL %s; isolated database %s", environment.Version, environment.DatabaseName)
	if err := db.AutoMigrate(&models.TaskTrace{}); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"idx_trace_tail", "idx_trace_latest", "idx_trace_execution"} {
		if err := db.Migrator().DropIndex(&models.TaskTrace{}, index); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrator().DropColumn(&models.TaskTrace{}, "execution_id"); err != nil {
		t.Fatal(err)
	}
	// Deliberately reset attempt/seq in legacy data: historical execution identity
	// cannot be inferred. Preserve every stored field and insertion ordering.
	for i := 0; i < 551; i++ {
		row := map[string]any{"task_id": 7, "attempt": i % 3, "seq": i % 5,
			"step": fmt.Sprintf("legacy-%03d", i), "status": int(models.TraceWarn),
			"kind": "historical", "message": "历史记录", "data": `{"kept":true}`,
			"duration": 123456, "created_at": time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)}
		if err := db.Table("crawler_task_trace").Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	var before []models.TaskTrace
	if err := db.Order("id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}

	if err := database.CheckTraceSchema(db); err == nil {
		t.Fatal("old schema reported ready")
	}
	cfg := &config.Config{}
	cfg.Crawler.Trace.Enabled = true
	for pass := 1; pass <= 2; pass++ {
		if err := database.Migrate(db, cfg); err != nil {
			t.Fatalf("migration %d: %v", pass, err)
		}
		if err := database.CheckTraceSchema(db); err != nil {
			t.Fatal(err)
		}
		var after []models.TaskTrace
		if err := db.Order("id").Find(&after).Error; err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("migration %d changed historical rows", pass)
		}
		var column struct{ ColumnType, IsNullable, ColumnDefault string }
		if err := db.Raw(`SELECT column_type, is_nullable, column_default FROM information_schema.columns
   WHERE table_schema=DATABASE() AND table_name='crawler_task_trace' AND column_name='execution_id'`).Scan(&column).Error; err != nil {
			t.Fatal(err)
		}
		if column.ColumnType != "varchar(36)" || column.IsNullable != "NO" || column.ColumnDefault != "" {
			t.Fatalf("column: %+v", column)
		}
		for index, want := range map[string][]string{
			"idx_task_attempt_seq": {"task_id", "attempt", "seq"},
			"idx_trace_execution":  {"task_id", "execution_id"},
			"idx_trace_latest":     {"task_id", "id"},
			"idx_trace_tail":       {"task_id", "execution_id", "attempt", "seq", "id"},
		} {
			var columns []string
			if err := db.Raw(`SELECT column_name FROM information_schema.statistics WHERE table_schema=DATABASE()
    AND table_name='crawler_task_trace' AND index_name=? ORDER BY seq_in_index`, index).Scan(&columns).Error; err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(columns, want) {
				t.Fatalf("index %s: %v, want %v", index, columns, want)
			}
		}
	}

	e := makeEngine()
	e.cfg.Crawler.Trace.Enabled = true
	legacy, err := e.ListTraceExecution(7)
	if err != nil || len(legacy) != 500 {
		t.Fatalf("legacy count=%d: %v", len(legacy), err)
	}
	for i, step := range legacy {
		if step.ExecutionID != "" || step.Step != fmt.Sprintf("legacy-%03d", i+51) || step.Truncated != (i == 0) {
			t.Fatalf("legacy order/metadata %d: %+v", i, step)
		}
	}

	rows := make([]models.TaskTrace, 551)
	for i := range rows {
		rows[i] = models.TaskTrace{TaskID: 7, ExecutionID: "new-execution", Attempt: i / 300, Seq: i % 300, Step: "step"}
	}
	rows[550].Status = models.TraceFailed
	if err := db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	steps, err := e.ListTraceExecution(7)
	if err != nil || len(steps) != 500 {
		t.Fatalf("tail size %d: %v", len(steps), err)
	}
	if steps[0].Seq != 51 || !steps[0].Truncated || steps[499].Status != "failed" || steps[499].Attempt != 1 || steps[499].Seq != 250 {
		t.Fatalf("wrong tail: %+v ... %+v", steps[0], steps[499])
	}
	for i, step := range steps {
		if step.ExecutionID != "new-execution" || step.Truncated != (i == 0) {
			t.Fatalf("mixed execution/metadata: %+v", step)
		}
	}
	oldAPI, err := e.ListTrace(7)
	if err != nil || len(oldAPI) != len(steps) {
		t.Fatalf("legacy API count=%d: %v", len(oldAPI), err)
	}
	for i := range oldAPI {
		if !reflect.DeepEqual(oldAPI[i], steps[i].TraceStep) {
			t.Fatalf("legacy API mismatch %d", i)
		}
	}
	// A newer short execution must not inherit the older execution's tail/truncation.
	newest := models.TaskTrace{TaskID: 7, ExecutionID: "newest-execution", Step: "newest", Data: datatypes.JSON(`{"new":true}`)}
	if err := db.Create(&newest).Error; err != nil {
		t.Fatal(err)
	}
	latest, err := e.ListTraceExecution(7)
	if err != nil || len(latest) != 1 || latest[0].ExecutionID != "newest-execution" || latest[0].Truncated {
		t.Fatalf("latest=%+v: %v", latest, err)
	}
	var count int64
	if err := db.Model(&models.TaskTrace{}).Count(&count).Error; err != nil || count != 1103 {
		t.Fatalf("stored count=%d: %v", count, err)
	}
	t.Log("verified two migrations, unchanged 551 legacy rows, schema/index definitions, 500-step tail across retries, legacy API and newest execution isolation")

}
