package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
	"strings"
	"testing"
	"time"
)

func TestTaskTimingRestoresDeadlineAndClaimSQL(t *testing.T) {
	at := time.Now().UTC().Add(time.Hour)
	e := &Engine{}
	task := e.taskFromRecord(&models.CrawlerTask{NotBefore: &at, DelayMS: 3600000, LastStartedAt: &at})
	if !task.NotBefore.Equal(at) || task.Delay != time.Hour || task.LastStartedAt == nil {
		t.Fatal("timing lost on restore", task)
	}
	sql := dryDB(t).ToSQL(func(tx *gorm.DB) *gorm.DB { return claimScope(tx, 1).Update("status", models.TaskStatusProcessing) })
	if !strings.Contains(sql, "not_before IS NULL OR not_before <= UTC_TIMESTAMP(3)") {
		t.Fatal(sql)
	}
	if taskDelayMillis(time.Nanosecond) != 1 {
		t.Fatal("submillisecond delay rounded down")
	}
}

func TestAttemptFinishPersistenceFailureNeverReplaysHandler(t *testing.T) {
	e, _, task, c := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub"}
	e.stages["stub:stub"].fetcher = fetcher
	e.stages["stub:stub"].config.MaxAttempts = 3
	if err := e.db.Callback().Update().Before("gorm:update").Register("test:finish-timing-failure", func(tx *gorm.DB) {
		if v, ok := tx.Statement.Dest.(map[string]any); ok && v["last_finished_at"] != nil {
			tx.AddError(ErrTaskChanged)
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := e.stageHandler(e.stages["stub:stub"])(context.Background(), task)
	drainTestEvents(t, e)
	if !errors.Is(err, errAttemptTimingPersistence) || len(fetcher.calls()) != 1 || countTaskEvents(c, events.TypeTaskFinished) != 0 {
		t.Fatalf("timing error replayed business or reported success: err=%v calls=%d", err, len(fetcher.calls()))
	}
	if _, ok := e.execution.Snapshot()[task.ID]; !ok {
		t.Fatal("lost persistence responsibility")
	}
}

func TestAttemptStartPersistenceFailureDoesNotCallHandler(t *testing.T) {
	e, _, task, _ := persistenceFixture(t)
	fetcher := &recordingFetcher{stage: "stub"}
	e.stages["stub:stub"].fetcher = fetcher
	if err := e.db.Callback().Update().Before("gorm:update").Register("test:start-timing-failure", func(tx *gorm.DB) {
		if v, ok := tx.Statement.Dest.(map[string]any); ok && v["last_started_at"] != nil {
			tx.AddError(ErrTaskChanged)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.stageHandler(e.stages["stub:stub"])(context.Background(), task); !errors.Is(err, errAttemptTimingPersistence) {
		t.Fatal(err)
	}
	if len(fetcher.calls()) != 0 {
		t.Fatal("handler ran without durable attempt start")
	}
}

func TestClaimRejectedByDurableDeadlineRetainsTask(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	f.affected = 0
	f.row["not_before"] = time.Now().UTC().Add(time.Hour)
	if claimed, err := e.claimTask(task); claimed || err != nil {
		t.Fatal(claimed, err)
	}
	if task.NotBefore.IsZero() || e.delays.Len() != 1 {
		t.Fatal("database-rejected pending task lost scheduling responsibility")
	}
}

func TestSubmitExistingTaskCannotReplacePersistedDeadline(t *testing.T) {
	e, f, task, _ := persistenceFixture(t)
	saved := time.Now().UTC().Add(time.Hour)
	f.row["not_before"], f.row["delay_ms"] = saved, int64(3600000)
	task.Repeatable = true
	task.NotBefore = time.Now().UTC().Add(-time.Hour)
	task.Delay = 2 * time.Hour
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if !task.NotBefore.Equal(saved) || task.Delay != time.Hour || e.delays.Len() != 1 {
		t.Fatal("duplicate changed persisted scheduling")
	}
}
