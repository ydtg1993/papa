package engine

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestTaskFlowMySQLSwitchRestartConcurrencyAndBoundaries(t *testing.T) {
	dsn := os.Getenv("PAPA_TASK_FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_TASK_FLOW_TEST_DSN not set")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_task_flow_test" {
		t.Fatal("requires dedicated papa_task_flow_test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open dedicated database")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err = db.AutoMigrate(&models.CrawlerTask{}, &models.TaskFlow{}); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	site := "flow-" + id
	defer db.Where("id = ?", id).Delete(&models.TaskFlow{})
	defer db.Where("site = ?", site).Delete(&models.CrawlerTask{})
	makeEngine := func() *Engine {
		cfg := &config.Config{}
		cfg.TaskFlow.Enabled = true
		e := NewEngine(db, cfg, testLoggerSet())
		t.Cleanup(e.cancel)
		for _, stage := range []string{"catalog", "detail", "content"} {
			key := core.StageKey(site, stage)
			e.stages[key] = &stageInfo{stage: stage, config: StageConfig{Site: site}}
			e.pools.set(key, workerpool.NewWorkerPool[*Task](1, 100, 1))
		}
		return e
	}
	e := makeEngine()
	ctx := context.Background()
	p := FlowPolicy{Stages: map[string]string{"catalog": "discover", "detail": "items", "content": "items"}, Lanes: map[string]FlowLane{"discover": {"breadth", 2}, "items": {"depth", 1}}}
	if err = e.CreateTaskFlow(ctx, id, site, p); err != nil {
		t.Fatal(err)
	}
	submit := func(parent uint, stage, path string) *Task {
		t.Helper()
		task := &Task{Site: site, Stage: stage, URL: "https://example.invalid/" + path}
		if err = e.SubmitFlowTask(ctx, id, parent, task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	claim := func(task *Task, want bool) {
		t.Helper()
		ok, err := e.claimTaskContext(ctx, task)
		if err != nil || ok != want {
			t.Fatalf("claim %d=%v err=%v want=%v", task.ID, ok, err, want)
		}
	}
	finish := func(task *Task) {
		t.Helper()
		if err = db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("status", models.TaskStatusSuccess).Error; err != nil {
			t.Fatal(err)
		}
		e.finishTask(task)
	}
	a := submit(0, "detail", "a")
	b := submit(0, "detail", "b")
	claim(b, false)
	claim(a, true)
	c := submit(uint(a.ID), "content", "a-content")
	claim(c, false)
	finish(a)
	claim(b, false)
	dup := submit(uint(a.ID), "content", "a-content")
	if dup.ID != c.ID {
		t.Fatal("duplicate child created")
	}
	if err = e.SubmitFlowTask(ctx, id, uint(a.ID), &Task{Site: site, Stage: "content", URL: "https://example.invalid/late"}); !errors.Is(err, ErrFlowConflict) {
		t.Fatal("late child accepted", err)
	}
	// Reconstruct the engine and task solely from durable state; no in-memory scheduler ownership.
	e = makeEngine()
	stored, err := e.loadTask(uint(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	c = e.taskFromRecord(stored)
	view, err := e.TaskFlow(ctx, id)
	if err != nil || view.Revision != 1 || len(view.Tasks) != 2 {
		t.Fatal("restore", view, err)
	}
	p.Lanes["items"] = FlowLane{"breadth", 2}
	if err = e.UpdateTaskFlow(ctx, id, 1, p); err != nil {
		t.Fatal(err)
	}
	if err = e.UpdateTaskFlow(ctx, id, 1, p); !errors.Is(err, ErrFlowConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	claim(c, false)
	claim(b, true)
	p.Lanes["items"] = FlowLane{"depth", 1}
	if err = e.UpdateTaskFlow(ctx, id, 2, p); err != nil {
		t.Fatal(err)
	}
	claim(c, false)
	finish(b)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := e.claimTaskContext(ctx, c)
			if err != nil {
				t.Error(err)
			}
			if ok {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatal("duplicate claim", won.Load())
	}
	finish(c)
	if err = e.DeleteTask(uint(c.ID)); !errors.Is(err, ErrTaskBatchProtected) {
		t.Fatal("lineage deleted", err)
	}
	if err = e.SetTaskRepeatable(uint(c.ID), true); !errors.Is(err, ErrTaskBatchProtected) {
		t.Fatal("flow polling enabled", err)
	}
	// Ordinary admission cannot strip managed identity and bypass the coordinator.
	if err = db.Model(&models.CrawlerTask{}).Where("id = ?", c.ID).Update("status", models.TaskStatusPending).Error; err != nil {
		t.Fatal(err)
	}
	stripped := *c
	stripped.FlowID = ""
	claim(&stripped, false)
	disabled := makeEngine()
	disabled.cfg.TaskFlow.Enabled = false
	if ok, err := disabled.claimTaskContext(ctx, c); ok || err != nil {
		t.Fatal("disabled flow executed", ok, err)
	}
	if disabled.spillBacklog() != 1 {
		t.Fatal("disabled task lost")
	}
	// Strategy update and concurrent admission serialize through the same run row.
	var changes atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.UpdateTaskFlow(ctx, id, 3, p)
			if err == nil {
				changes.Add(1)
			} else if !errors.Is(err, ErrFlowConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if changes.Load() != 1 {
		t.Fatal("concurrent update", changes.Load())
	}
	finish(c)
	// Exercise the real stage handler: blocked tasks return immediately, child
	// registration precedes parent terminal state, and terminal notification wakes drain.
	var order []string
	children := map[int]*Task{}
	for _, stage := range []string{"detail", "content"} {
		info := e.stages[core.StageKey(site, stage)]
		info.config.MaxAttempts = 1
		info.fetcher = flowStageFetcher{stage: stage, run: func(ctx context.Context, task *Task, eng *Engine) error {
			order = append(order, task.URL)
			if task.Stage == "detail" {
				child := &Task{Site: site, Stage: "content", URL: task.URL + "/body"}
				if err := eng.SubmitFlowTask(ctx, id, uint(task.ID), child); err != nil {
					return err
				}
				children[task.ID] = child
			}
			return nil
		}}
	}
	x := submit(0, "detail", "x")
	y := submit(0, "detail", "y")
	run := func(task *Task) {
		t.Helper()
		if err := e.stageHandler(e.stages[core.StageKey(site, task.Stage)])(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	run(y)
	if len(order) != 0 {
		t.Fatal("blocked branch executed")
	}
	run(x)
	run(y)
	run(children[x.ID])
	run(y)
	run(children[y.ID])
	want := []string{x.URL, x.URL + "/body", y.URL, y.URL + "/body"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("stage order=%v want=%v", order, want)
	}
}

type flowStageFetcher struct {
	stage string
	run   func(context.Context, *Task, *Engine) error
}

func (f flowStageFetcher) GetStage() string { return f.stage }
func (f flowStageFetcher) FetchHandler(ctx context.Context, t *Task, e *Engine) error {
	return f.run(ctx, t, e)
}
