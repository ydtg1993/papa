package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/models"
)

func TestPendingSchedulerMySQLDownstreamChildrenAndIndependentStage(t *testing.T) {
	for _, flow := range []bool{false, true} {
		t.Run(fmt.Sprintf("flow_%v", flow), func(t *testing.T) {
			db, _ := compensationMySQL(t)
			if err := db.AutoMigrate(&models.CrawlerTask{}, &models.TaskFlow{}); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{}
			cfg.TaskFlow.Enabled = flow
			e := NewEngine(db, cfg, testLoggerSet())
			if err := e.SetSchedulerLimits(SchedulerLimits{Global: 4, Site: 4, Stage: 2, RefillBatch: 2}); err != nil {
				t.Fatal(err)
			}
			site, id := "synthetic", uuid.NewString()
			childrenAccepted, release := make(chan error, 1), make(chan struct{})
			var calls atomic.Int64
			handler := flowStageFetcher{stage: "main", run: func(ctx context.Context, task *Task, eng *Engine) error {
				calls.Add(1)
				if task.URL != "https://example.invalid/root" {
					return nil
				}
				var err error
				for i := 0; i < 12; i++ {
					child := &Task{Site: site, Stage: "detail", URL: fmt.Sprintf("https://example.invalid/child/%d", i)}
					if flow {
						err = eng.SubmitFlowTask(ctx, id, uint(task.ID), child)
					} else {
						err = eng.SubmitChildren(ctx, task, child)
					}
					if err != nil {
						break
					}
				}
				childrenAccepted <- err
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				return err
			}}
			independent := make(chan struct{}, 1)
			if err := e.AddStage("detail", StageConfig{Site: site, MaxAttempts: 1, WorkerCount: 1, QueueSize: 1}, flowStageFetcher{stage: "detail", run: func(context.Context, *Task, *Engine) error { calls.Add(1); return nil }}, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.AddStage("main", StageConfig{Site: site, Entry: true, MaxAttempts: 1, WorkerCount: 1, QueueSize: 1, Downstream: []string{"detail"}}, handler, nil); err != nil {
				t.Fatal(err)
			}
			if err := e.AddStage("other", StageConfig{Site: site, Entry: true, MaxAttempts: 1, WorkerCount: 1, QueueSize: 1}, flowStageFetcher{stage: "other", run: func(context.Context, *Task, *Engine) error { independent <- struct{}{}; return nil }}, nil); err != nil {
				t.Fatal(err)
			}
			if flow {
				if err := e.CreateTaskFlow(context.Background(), id, site, FlowPolicy{Stages: map[string]string{"main": "serial", "detail": "serial"}, Lanes: map[string]FlowLane{"serial": {Mode: "breadth", Parallel: 1}}}); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { e.Stop(time.Second) })
			root := &Task{Site: site, Stage: "main", URL: "https://example.invalid/root"}
			var err error
			if flow {
				err = e.SubmitFlowTask(context.Background(), id, 0, root)
			} else {
				err = e.SubmitTask(root)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := e.SubmitTask(&Task{Site: site, Stage: "other", URL: "https://example.invalid/independent"}); err != nil {
				t.Fatal(err)
			}
			e.ApplyRegisterStage()
			select {
			case err := <-childrenAccepted:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker blocked submitting downstream children")
			}
			if got := e.SchedulerResidentCount(); got > 4 {
				t.Fatalf("resident bound exceeded: %d", got)
			}
			if pool := e.pools.Get(core.StageKey(site, "main")); pool == nil {
				t.Fatal("main pool absent")
			} else if main, urgent := pool.QueueDepths(); main > 1 || urgent != 0 {
				t.Fatalf("queue bounds %d/%d", main, urgent)
			}
			select {
			case <-independent:
			case <-time.After(5 * time.Second):
				t.Fatal("full stage blocked independent dispatcher")
			}
			close(release)
			deadline := time.Now().Add(10 * time.Second)
			for {
				var count int64
				if err := db.Model(&models.CrawlerTask{}).Where("site = ? AND status = ?", site, models.TaskStatusSuccess).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count == 14 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("terminal successes=%d calls=%d; pending children lost", count, calls.Load())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if calls.Load() != 13 {
				t.Fatalf("duplicate execution: %d", calls.Load())
			}
		})
	}
}

func TestPendingSchedulerMySQLDeadlinesPauseAndRestart(t *testing.T) {
	for _, scenario := range []string{"deadline", "paused_full_queue", "restart"} {
		t.Run(scenario, func(t *testing.T) {
			db, _ := compensationMySQL(t)
			if err := db.AutoMigrate(&models.CrawlerTask{}); err != nil {
				t.Fatal(err)
			}
			const site = "synthetic"
			var calls atomic.Int64
			called := make(chan string, 8)
			release := make(chan struct{})
			newEngine := func() *Engine {
				e := NewEngine(db, &config.Config{}, testLoggerSet())
				e.SetSite(core.Site{Key: site})
				e.SetSiteBreaker(site, breaker.Config{Enabled: false, Threshold: 1})
				if err := e.AddStage("main", StageConfig{Site: site, Entry: true, MaxAttempts: 1, WorkerCount: 1, QueueSize: 1}, flowStageFetcher{stage: "main", run: func(ctx context.Context, task *Task, _ *Engine) error {
					calls.Add(1)
					called <- task.URL
					if scenario == "paused_full_queue" && task.URL == "https://example.invalid/first" {
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				}}, nil); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { e.Stop(time.Second) })
				return e
			}
			e := newEngine()
			submit := func(url string, at time.Time) {
				t.Helper()
				if err := e.SubmitTask(&Task{Site: site, Stage: "main", URL: url, NotBefore: at}); err != nil {
					t.Fatal(err)
				}
			}
			want := int64(1)
			switch scenario {
			case "deadline":
				at := time.Now().UTC().Add(700 * time.Millisecond)
				submit("https://example.invalid/future", at)
				e.ApplyRegisterStage()
				d, err := taskstore.NextPendingDelay(context.Background(), db, site, "main")
				if err != nil || d <= 0 || d > time.Second {
					t.Fatalf("persisted deadline delay=%v err=%v", d, err)
				}
				select {
				case <-called:
					t.Fatal("future task ran before deadline")
				case <-time.After(150 * time.Millisecond):
				}
				select {
				case <-called:
					if time.Now().UTC().Before(at.Add(-5 * time.Millisecond)) {
						t.Fatal("task ran before durable deadline")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("deadline timer failed to wake exhausted scan")
				}
			case "paused_full_queue":
				want = 3
				submit("https://example.invalid/first", time.Time{})
				submit("https://example.invalid/second", time.Time{})
				submit("https://example.invalid/third", time.Time{})
				e.ApplyRegisterStage()
				select {
				case <-called:
				case <-time.After(3 * time.Second):
					t.Fatal("first handler did not start")
				}
				deadline := time.Now().Add(3 * time.Second)
				for {
					pool := e.pools.Get(core.StageKey(site, "main"))
					if pool != nil {
						main, _ := pool.QueueDepths()
						if main == 1 {
							break
						}
					}
					if time.Now().After(deadline) {
						t.Fatal("queue never filled")
					}
					time.Sleep(time.Millisecond)
				}
				if !e.PauseSite(site, "test paused full queue") {
					t.Fatal("pause failed")
				}
				close(release)
				select {
				case url := <-called:
					t.Fatalf("paused worker executed %s", url)
				case <-time.After(100 * time.Millisecond):
				}
				if !e.ResumeSite(site) {
					t.Fatal("resume failed")
				}
			case "restart":
				submit("https://example.invalid/restart", time.Now().Add(time.Hour))
				e.ApplyRegisterStage()
				if !e.PauseSite(site, "hold durable work for restart") {
					t.Fatal("pause failed")
				}
				e.Stop(time.Second)
				if err := db.Model(&models.CrawlerTask{}).Where("site = ?", site).Update("not_before", nil).Error; err != nil {
					t.Fatal(err)
				}
				var n int64
				if err := db.Model(&models.CrawlerTask{}).Where("site = ? AND status = ?", site, models.TaskStatusPending).Count(&n).Error; err != nil || n != 1 {
					t.Fatalf("stop lost durable pending: n=%d err=%v", n, err)
				}
				e = newEngine()
				e.ApplyRegisterStage()
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var n int64
				if err := db.Model(&models.CrawlerTask{}).Where("site = ? AND status = ?", site, models.TaskStatusSuccess).Count(&n).Error; err != nil {
					t.Fatal(err)
				}
				if n == want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("success=%d calls=%d want=%d", n, calls.Load(), want)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if calls.Load() != want {
				t.Fatalf("duplicate or lost handler calls=%d want=%d", calls.Load(), want)
			}
		})
	}
}
