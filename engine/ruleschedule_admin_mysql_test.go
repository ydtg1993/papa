package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func scheduleAdminEngine(db *gorm.DB, s *RuleSchedule, task *Task) *Engine {
	return &Engine{db: db, stages: map[string]*stageInfo{core.StageKey(task.Site, task.Stage): {stage: task.Stage, fetcher: s}}}
}

func TestRuleScheduleAdminMySQLLifecycleAndPagination(t *testing.T) {
	db, s, _, task := scheduleMySQL(t)
	e := scheduleAdminEngine(db, s, task)
	ctx := context.Background()
	before, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil || before.Batch.Sequence != 0 || len(before.Members) != 0 {
		t.Fatalf("unstarted: %+v %v", before, err)
	}
	if err = scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	first, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil || first.Batch.Total != 1 || len(first.Rules) == 0 || first.Batch.ReleaseID != "release-1" {
		t.Fatalf("first: %+v %v", first, err)
	}
	for i := 0; i < 52; i++ {
		child := &Task{Site: task.Site, Stage: "detail", URL: fmt.Sprintf("https://example.com/member/%d", i), Meta: map[string]string{ruleBatchMeta: first.Batch.BatchID}}
		if err = child.Insert(db); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err = db.Model(&models.CrawlerTask{}).Where("id = ?", child.ID).Update("status", models.TaskStatusFailed).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	detail, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, first.Batch.BatchID, false)
	if err != nil || detail.Batch.Total != 53 || detail.Batch.Failed != 26 || len(detail.Members) != 50 || detail.Next == 0 {
		t.Fatalf("members: %+v %v", detail, err)
	}
	next, err := e.RuleScheduleDetail(ctx, uint(task.ID), detail.Next, first.Batch.BatchID, false)
	if err != nil || len(next.Members) != 3 || next.Next != 0 || next.Members[0].ID <= detail.Next {
		t.Fatalf("next: %+v %v", next, err)
	}
	failed, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, first.Batch.BatchID, true)
	if err != nil || len(failed.Members) != 26 {
		t.Fatalf("failed: %+v %v", failed, err)
	}
	for _, member := range failed.Members {
		if member.Status != models.TaskStatusFailed {
			t.Fatal("failure filter leaked other status")
		}
	}
	if _, err = e.RuleScheduleDetail(ctx, uint(task.ID), detail.Next, strings.Repeat("f", 64), false); !errors.Is(err, ErrRuleScheduleConflict) {
		t.Fatalf("stale batch: %v", err)
	}
	if _, err = e.RuleScheduleDetail(ctx, uint(task.ID), 1, "", false); !errors.Is(err, ErrRuleScheduleInvalid) {
		t.Fatalf("unbound cursor: %v", err)
	}
	// New Engine instance sees the same durable state without in-memory history.
	restored, err := scheduleAdminEngine(db, s, task).RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil || restored.Batch.BatchID != first.Batch.BatchID || restored.Batch.Total != 53 {
		t.Fatalf("restored: %+v %v", restored, err)
	}
	for i := 0; i < 21; i++ {
		control, err := s.NewTask(RuleRequest{URL: "https://example.com/list"}, map[string]string{"job": fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
		if err = control.Insert(db); err != nil {
			t.Fatal(err)
		}
	}
	page, err := e.ListRuleSchedules(ctx, task.Site, 0)
	if err != nil || len(page.Items) != 20 || page.Next == 0 {
		t.Fatalf("controllers: %+v %v", page, err)
	}
	last, err := e.ListRuleSchedules(ctx, task.Site, page.Next)
	if err != nil || len(last.Items) != 2 || last.Next != 0 {
		t.Fatalf("last: %+v %v", last, err)
	}
	delete(e.stages, core.StageKey(task.Site, task.Stage))
	if _, err = e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false); !errors.Is(err, ErrStageNotRegistered) {
		t.Fatalf("unregistered: %v", err)
	}
	page, err = e.ListRuleSchedules(ctx, task.Site, 0)
	if err != nil || len(page.Items) != 20 {
		t.Fatal("unregistered controllers must remain discoverable")
	}
	e = scheduleAdminEngine(db, s, task)
	if err = db.Model(&models.RuleSchedule{}).Where("task_id = ?", task.ID).Update("payload_sha256", strings.Repeat("0", 64)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false); !errors.Is(err, ErrRuleScheduleCorrupt) {
		t.Fatalf("corruption hidden: %v", err)
	}
}

func TestRuleScheduleAdminMySQLConcurrentSnapshot(t *testing.T) {
	db, s, _, task := scheduleMySQL(t)
	e := scheduleAdminEngine(db, s, task)
	ctx := context.Background()
	if err := scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	old, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	type marker struct{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err = db.Callback().Query().After("gorm:query").Register("test:snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_rule_schedules" && tx.Statement.Context.Value(marker{}) == true {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-tx.Statement.Context.Done():
				}
			})
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove("test:snapshot")
	type result struct {
		detail RuleScheduleDetail
		err    error
	}
	done := make(chan result, 1)
	go func() {
		d, err := e.RuleScheduleDetail(context.WithValue(ctx, marker{}, true), uint(task.ID), 0, "", false)
		done <- result{d, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("snapshot did not start")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", old.Batch.RootID).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	scheduleUpdateTick(t, db, task, 10)
	if err = scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	close(release)
	r := <-done
	if r.err != nil || r.detail.Batch.BatchID != old.Batch.BatchID || r.detail.Batch.Pending != 1 || r.detail.Members[0].Status != models.TaskStatusPending {
		t.Fatalf("mixed snapshot: %+v %v", r.detail, r.err)
	}
	now, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil || now.Batch.Sequence != 2 || now.Batch.BatchID == old.Batch.BatchID {
		t.Fatalf("new state invisible: %+v %v", now, err)
	}
}

func TestRuleBatchAdminMySQLConcurrentRetry(t *testing.T) {
	db, s, _, task := scheduleMySQL(t)
	e := scheduleAdminEngine(db, s, task)
	ctx := context.Background()
	if err := scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	detail, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	root := detail.Batch.RootID
	for _, id := range []uint{uint(task.ID), root} {
		if err = e.DeleteTask(id); !errors.Is(err, ErrTaskBatchProtected) {
			t.Fatalf("delete %d: %v", id, err)
		}
	}
	if err = e.SetTaskRepeatable(root, true); !errors.Is(err, ErrTaskBatchProtected) {
		t.Fatal(err)
	}
	if err = e.SetTaskRepeatInterval(root, 0, 60); !errors.Is(err, ErrTaskBatchProtected) {
		t.Fatal(err)
	}
	if err = e.RetryTask(root, 0); !errors.Is(err, ErrTaskBatchProtected) {
		t.Fatal(err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", root).Update("status", models.TaskStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	// Queue without running workers makes the winning pending state observable.
	e, pool := urgentEngine(t, newFakeTaskDB())
	e.db = db
	var row models.CrawlerTask
	if err = db.First(&row, root).Error; err != nil {
		t.Fatal(err)
	}
	e.stages[stageKeyOfRecord(&row)] = &stageInfo{stage: row.Stage, config: StageConfig{Site: row.Site}}
	e.pools.set(stageKeyOfRecord(&row), pool)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { errs <- e.RetryTask(root, 0) }()
	}
	winners := 0
	for i := 0; i < 8; i++ {
		err := <-errs
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrTaskChanged) && !errors.Is(err, ErrTaskBatchProtected) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("retry winners=%d", winners)
	}
	if err = db.First(&row, root).Error; err != nil || row.Status != models.TaskStatusPending || row.Reprocess != 1 {
		t.Fatalf("retry row: %+v %v", row, err)
	}
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", root).Update("status", models.TaskStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	if err = e.RetryTask(root, 1); !errors.Is(err, ErrTaskBatchProtected) {
		t.Fatalf("historical success replayed: %v", err)
	}
}

func TestRuleBatchAdminMySQLRetryChecksLatestState(t *testing.T) {
	db, s, _, task := scheduleMySQL(t)
	e := scheduleAdminEngine(db, s, task)
	ctx := context.Background()
	if err := scheduleRun(t, s, ctx, task, e, scheduleInsertRoot(db)); err != nil {
		t.Fatal(err)
	}
	detail, err := e.RuleScheduleDetail(ctx, uint(task.ID), 0, "", false)
	if err != nil {
		t.Fatal(err)
	}
	root := detail.Batch.RootID
	e, pool := urgentEngine(t, newFakeTaskDB())
	var row models.CrawlerTask
	if err = db.First(&row, root).Error; err != nil {
		t.Fatal(err)
	}
	e.stages[stageKeyOfRecord(&row)] = &stageInfo{stage: row.Stage, config: StageConfig{Site: row.Site}}
	e.pools.set(stageKeyOfRecord(&row), pool)
	// Interleave a competing state update after the admin read, before its CAS.
	type marker struct{}
	e.db = db.WithContext(context.WithValue(ctx, marker{}, true))
	for _, changed := range []map[string]any{{"status": models.TaskStatusSuccess}, {"repeatable": models.RepeatableYes}} {
		if err = db.Model(&models.CrawlerTask{}).Where("id = ?", root).Updates(map[string]any{"status": models.TaskStatusFailed, "repeatable": models.RepeatableNo}).Error; err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		if err = db.Callback().Update().Before("gorm:update").Register("test:retry-state", func(tx *gorm.DB) {
			if tx.Statement.Context.Value(marker{}) == true {
				once.Do(func() { close(entered); <-release })
			}
		}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.RetryTask(root, 0) }()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("retry did not reach update")
		}
		err = db.Model(&models.CrawlerTask{}).Where("id = ?", root).Updates(changed).Error
		close(release)
		retryErr := <-done
		db.Callback().Update().Remove("test:retry-state")
		if err != nil {
			t.Fatal(err)
		}
		if !errors.Is(retryErr, ErrTaskChanged) {
			t.Fatalf("stale retry: %v", retryErr)
		}
		if err = db.First(&row, root).Error; err != nil || row.Reprocess != 0 {
			t.Fatalf("stale retry changed version: %+v %v", row, err)
		}
	}
}
