package app

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/models"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"reflect"
	"testing"
	"time"
)

type executionPolicyEntry struct{ err error }

func (f *executionPolicyEntry) GetStage() string { return "detail" }
func (f *executionPolicyEntry) FetchHandler(context.Context, *engine.Task, *engine.Engine) error {
	return nil
}
func (f *executionPolicyEntry) SubmitEntries(e *engine.Engine, site core.Site) {
	f.err = e.SubmitTask(&engine.Task{Site: site.Key, Stage: "detail", URL: "https://example.invalid/pending"})
}

type executionCommitGate struct {
	ep.Store
	committed chan struct{}
	release   chan struct{}
}

func (s *executionCommitGate) Save(ctx context.Context, u ep.Update, operator string) (ep.History, error) {
	h, err := s.Store.Save(ctx, u, operator)
	if err == nil {
		close(s.committed)
		select {
		case <-s.release:
		case <-ctx.Done():
			return h, ctx.Err()
		}
	}
	return h, err
}

func TestExecutionPolicyMySQLPauseAndShutdownPreserveDelivery(t *testing.T) {
	db, _ := appMixDatabase(t, false)
	ctx := context.Background()
	cfg := &config.Config{}
	logs := &loggers.LoggerSet{Engine: quietLogger(), DB: quietLogger(), Monitor: quietLogger(), Sys: quietLogger()}
	e := engine.NewEngine(db, cfg, logs)
	t.Cleanup(func() { e.Stop(time.Second) })
	a := &App{DB: db, Config: cfg, Engine: e, Logger: logs}
	entry := &executionPolicyEntry{}
	a.RegisterSites(SiteSpec{Key: "fixture", StartPaused: true, Stages: []StageSpec{{Fetcher: entry, Entry: true, WorkerCount: 1, QueueSize: 2}}})
	store := configstore.ExecutionStore{Store: configstore.New(db)}
	doc := ep.InitialDocument()
	if _, err := store.Save(ctx, ep.Update{OperationID: "bootstrap-fixture", Document: doc, Reason: "fixture"}, "test"); err != nil {
		t.Fatal(err)
	}
	controller := ep.New(store, executionTarget{breakers: e}, []string{"fixture"})
	if err := controller.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	e.ApplyRegisterStage()
	if !e.BreakerStatuses()["fixture"].Paused {
		t.Fatal("initial manual pause absent")
	}
	if entry.err != nil {
		t.Fatal(entry.err)
	}
	failed := models.CrawlerTask{Site: "fixture", Stage: "detail", URL: "https://example.invalid/failed", Status: models.TaskStatusFailed, Retry: 4, Repeat: 2, Reprocess: 3, CompensationVersion: 7}
	if err := db.Create(&failed).Error; err != nil {
		t.Fatal(err)
	}
	var before []models.CrawlerTask
	if err := db.Order("id ASC").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("tasks=%d", len(before))
	}
	next := doc.Clone()
	next.Breaker.Defaults.Threshold = 17
	view, err := controller.Update(ctx, ep.Update{ExpectedRevision: 1, OperationID: "paused-update", Document: next, Reason: "fixture"}, "test")
	if err != nil || view.AppliedRevision != 2 {
		t.Fatalf("update %+v %v", view, err)
	}
	if !e.BreakerStatuses()["fixture"].Paused || e.BreakerStatuses()["fixture"].Threshold != 17 {
		t.Fatal("update altered manual pause or missed target")
	}
	var after []models.CrawlerTask
	if err = db.Order("id ASC").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("policy update changed task status/retry/generation/delivery")
	}
	// Commit has completed but publication is deliberately held until shutdown.
	gate := &executionCommitGate{Store: store, committed: make(chan struct{}), release: make(chan struct{})}
	stopped := ep.New(gate, executionTarget{breakers: e}, []string{"fixture"})
	if err = stopped.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	third := next.Clone()
	third.Breaker.Defaults.Threshold = 23
	result := make(chan error, 1)
	go func() {
		_, err := stopped.Update(ctx, ep.Update{ExpectedRevision: 2, OperationID: "shutdown-update", Document: third, Reason: "fixture"}, "test")
		result <- err
	}()
	select {
	case <-gate.committed:
	case <-time.After(3 * time.Second):
		t.Fatal("commit did not complete")
	}
	stopped.BeginShutdown()
	close(gate.release)
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown update did not finish")
	}
	state, err := stopped.Read(ctx)
	if err != nil || state.Revision != 3 || state.AppliedRevision != 2 || state.Status != "stopping" {
		t.Fatalf("shutdown status %+v %v", state, err)
	}
	if e.BreakerPolicyStatuses()["fixture"].TargetRevision != 2 {
		t.Fatal("published after shutdown fence")
	}
	if _, err = stopped.Update(ctx, ep.Update{ExpectedRevision: 3, OperationID: "after-shutdown", Document: third}, "test"); err == nil {
		t.Fatal("write accepted after stopping")
	}
	if drained, stats := e.Stop(time.Second); drained || stats["fixture:detail"].Queued != 1 {
		t.Fatalf("shutdown must report retained pending work: drained=%v stats=%+v", drained, stats)
	}
	if err = db.Order("id ASC").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("shutdown changed pending/failed task generations")
	}
	// The committed target is recovered by the next process, never falsely marked applied.
	restarted := engine.NewEngine(db, cfg, logs)
	t.Cleanup(func() { restarted.Stop(time.Second) })
	restarted.RegisterBreakerSite("fixture")
	loaded := ep.New(store, executionTarget{breakers: restarted}, []string{"fixture"})
	if err = loaded.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if restarted.BreakerPolicyStatuses()["fixture"].TargetRevision != 3 || restarted.BreakerStatuses()["fixture"].Threshold != 23 {
		t.Fatal("restart missed committed target")
	}
}
