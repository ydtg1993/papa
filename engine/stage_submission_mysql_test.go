package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func TestStageFlowMySQLRejectsInvalidEdgesBeforeSQL(t *testing.T) {
	db, _ := compensationMySQL(t)
	if err := db.AutoMigrate(&models.TaskFlow{}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.TaskFlow.Enabled = true
	e := NewEngine(db, cfg, testLoggerSet())
	t.Cleanup(func() { e.Stop(time.Second) })
	for _, stage := range []string{"root", "detail", "sibling", "leaf"} {
		c := StageConfig{Site: "synthetic", Entry: stage == "root"}
		if stage == "root" {
			c.Downstream = []string{"detail", "sibling"}
		}
		if stage == "detail" {
			c.Downstream = []string{"leaf"}
		}
		e.stages[core.StageKey("synthetic", stage)] = &stageInfo{stage: stage, config: c}
	}
	if err := e.sealStageSubmissions(); err != nil {
		t.Fatal(err)
	}
	parent := &Task{ID: 17, Site: "synthetic", Stage: "detail", URL: "https://example.invalid/parent"}
	ctx, finish := e.bindTaskExecution(context.Background(), parent)
	defer finish()
	var calls atomic.Int64
	const cb = "test:stage_flow_sql"
	if err := db.Callback().Query().Before("gorm:query").Register(cb, func(*gorm.DB) { calls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Create().Before("gorm:create").Register(cb, func(*gorm.DB) { calls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(cb); db.Callback().Create().Remove(cb) })
	for _, tc := range []struct{ name, site, stage string }{
		{"self", "synthetic", "detail"}, {"sibling", "synthetic", "sibling"}, {"reverse", "synthetic", "root"}, {"cross_site", "other", "leaf"}, {"unknown", "synthetic", "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := &Task{Site: tc.site, Stage: tc.stage, URL: "https://example.invalid/" + tc.name}
			if err := e.SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskFlow) {
				t.Fatalf("ordinary rejection=%v", err)
			}
			flowParent := *parent
			flowParent.FlowID = "test-flow"
			flowCtx, done := e.bindTaskExecution(context.Background(), &flowParent)
			defer done()
			if err := e.SubmitFlowTask(flowCtx, "test-flow", uint(flowParent.ID), child); !errors.Is(err, ErrTaskFlow) {
				t.Fatalf("flow rejection=%v", err)
			}
		})
	}
	finish()
	child := &Task{Site: "synthetic", Stage: "leaf", URL: "https://example.invalid/late"}
	if err := e.SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskFlow) {
		t.Fatal("expired permit accepted", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected submission reached SQL: %d", calls.Load())
	}
	db.Callback().Query().Remove(cb)
	db.Callback().Create().Remove(cb)
	var count int64
	if err := db.Model(&models.CrawlerTask{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rejected tasks persisted: %d %v", count, err)
	}
}

// Use the public execution path so the submission permit comes from a real
// claimed handler, not a test-created context value.
func TestStageFlowMySQLRejectsExistingIDAndFlowOwnershipBypass(t *testing.T) {
	db, _ := compensationMySQL(t)
	e := NewEngine(db, &config.Config{}, testLoggerSet())
	t.Cleanup(func() { e.Stop(time.Second) })
	flowRecord := (&Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/managed", Meta: map[string]string{"owner": "original"}, FlowID: "managed-flow"}).toModel()
	if err := db.Create(&flowRecord).Error; err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	handler := flowStageFetcher{stage: "root", run: func(ctx context.Context, parent *Task, eng *Engine) error {
		cases := []struct {
			name     string
			child    *Task
			storedID uint
		}{
			{"root ID disguised as downstream", &Task{ID: parent.ID, Site: parent.Site, Stage: "detail", URL: "https://example.invalid/unmatched-root", Meta: map[string]string{"owner": "changed"}}, uint(parent.ID)},
			{"flow ID disguised as ordinary", &Task{ID: int(flowRecord.ID), Site: parent.Site, Stage: "detail", URL: "https://example.invalid/unmatched-flow", Meta: map[string]string{"owner": "changed"}}, flowRecord.ID},
			{"flow identity disguised as ordinary", &Task{Site: parent.Site, Stage: "detail", URL: flowRecord.URL, Meta: map[string]string{"owner": "changed"}}, flowRecord.ID},
		}
		for _, tc := range cases {
			var before, after models.CrawlerTask
			if err := db.First(&before, tc.storedID).Error; err != nil {
				result <- err
				return err
			}
			err := eng.SubmitChildren(ctx, parent, tc.child)
			if !errors.Is(err, ErrTaskOwnershipConflict) {
				err = fmt.Errorf("%s: expected ownership conflict, got %v", tc.name, err)
				result <- err
				return err
			}
			if err := db.First(&after, tc.storedID).Error; err != nil {
				result <- err
				return err
			}
			if !reflect.DeepEqual(before, after) {
				err = fmt.Errorf("%s mutated the persisted owner record", tc.name)
				result <- err
				return err
			}
		}
		result <- nil
		return nil
	}}
	if err := e.AddStage("root", StageConfig{Site: "synthetic", Entry: true, Downstream: []string{"detail"}, WorkerCount: 1, QueueSize: 1, MaxAttempts: 1}, handler, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.AddStage("detail", StageConfig{Site: "synthetic", WorkerCount: 1, QueueSize: 1, MaxAttempts: 1}, flowStageFetcher{stage: "detail", run: func(context.Context, *Task, *Engine) error { return nil }}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.SubmitTask(&Task{Site: "synthetic", Stage: "root", URL: "https://example.invalid/root-owner", Meta: map[string]string{"owner": "root"}}); err != nil {
		t.Fatal(err)
	}
	e.ApplyRegisterStage()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root handler did not complete ownership checks")
	}
	var count int64
	if err := db.Model(&models.CrawlerTask{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("bypass created rows: %d %v", count, err)
	}
}
