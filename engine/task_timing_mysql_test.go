package engine

import (
	"context"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestTaskTimingMySQLPersistenceRecoveryAndAttempts(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	task := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/delayed", Delay: time.Hour, Repeatable: true}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	var row models.CrawlerTask
	if err := db.First(&row, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.NotBefore == nil || row.DelayMS != 3600000 {
		t.Fatal("delay not stored", row.NotBefore, row.DelayMS)
	}
	var delta int64
	if err := db.Raw("SELECT TIMESTAMPDIFF(SECOND,UTC_TIMESTAMP(3),not_before) FROM crawler_tasks WHERE id = ?", task.ID).Scan(&delta).Error; err != nil || delta < 3590 || delta > 3600 {
		t.Fatal("deadline not based on database clock", delta, err)
	}
	before := *row.NotBefore
	duplicate := &Task{Site: task.Site, Stage: task.Stage, URL: task.URL, Repeatable: true, Delay: 2 * time.Hour}
	if err := e.SubmitTask(duplicate); err != nil {
		t.Fatal(err)
	}
	if !duplicate.NotBefore.Equal(before) {
		t.Fatal("duplicate moved deadline", duplicate.NotBefore, before)
	}
	restored := makeEngine()
	if err := restored.submitRecoveredTask(restored.taskFromRecord(&row)); err != nil {
		t.Fatal(err)
	}

	n := restored.delays.Len()

	if n != 0 {
		t.Fatal("far-future task must stay durable outside the near window", n)
	}
	// An early in-memory copy must still be rejected and retained by the DB guard.
	early := restored.taskFromRecord(&row)
	early.NotBefore = time.Time{}
	if claimed, err := restored.claimTask(early); err != nil || claimed {
		t.Fatal("early claim", claimed, err)
	}
	if early.NotBefore.IsZero() {
		t.Fatal("early copy lost durable deadline")
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("not_before", gorm.Expr("DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND)")).Error; err != nil {
		t.Fatal(err)
	}
	var due models.CrawlerTask
	if err := db.First(&due, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	run := restored.taskFromRecord(&due)
	fetcher := &recordingFetcher{stage: "detail"}
	restored.stages["synthetic:detail"].fetcher = fetcher
	if err := restored.stageHandler(restored.stages["synthetic:detail"])(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(fetcher.calls()) != 1 || row.LastStartedAt == nil || row.LastFinishedAt == nil || row.LastFinishedAt.Before(*row.LastStartedAt) || row.Status != models.TaskStatusSuccess {
		t.Fatalf("attempt timing missing: calls=%d row=%+v", len(fetcher.calls()), row)
	}
}

func TestTaskTimingMySQLBatchExplicitDeadlineAndNegativeDelay(t *testing.T) {
	location, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	db, makeEngine := compensationMySQLWithLocation(t, location)
	// A non-UTC MySQL session must not shift these UTC-only fields. The driver's
	// Loc remains whatever the connection fixture uses; the serializer isolates it.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.Exec("SET time_zone = '+08:00'").Error; err != nil {
		t.Fatal(err)
	}
	e := makeEngine()
	at := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Millisecond)
	tasks := []*Task{
		{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/relative", Delay: time.Hour},
		{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/absolute", Delay: time.Hour, NotBefore: at},
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatal(err)
	}
	for i, task := range tasks {
		var row models.CrawlerTask
		if err := db.First(&row, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if row.NotBefore == nil || row.DelayMS != 3600000 {
			t.Fatal("batch timing lost")
		}
		if i == 0 && (row.NotBefore.Sub(time.Now().UTC()) < 59*time.Minute || row.NotBefore.Sub(time.Now().UTC()) > 61*time.Minute) {
			t.Fatal("session time_zone shifted deadline", row.NotBefore)
		}
		if i == 1 && !row.NotBefore.Equal(at) {
			t.Fatal("explicit deadline did not win", row.NotBefore, at)
		}
	}
	bad := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/bad", Delay: -time.Nanosecond}
	if err := e.SubmitTask(bad); err == nil || bad.ID != 0 {
		t.Fatal("negative delay accepted", err)
	}
}
