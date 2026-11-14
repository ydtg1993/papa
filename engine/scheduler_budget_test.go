package engine

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func TestResidentTransfersAndDuplicateRelease(t *testing.T) {
	r := newResidentSet(SchedulerLimits{Global: 2, Site: 1, Stage: 1})
	a := &Task{ID: 1, Site: "a", Stage: "s"}
	b := &Task{ID: 2, Site: "a", Stage: "s"}
	c := &Task{ID: 3, Site: "b", Stage: "s"}
	if !r.admit(a, "delivery") || r.admit(b, "pool") || !r.admit(c, "delay") {
		t.Fatal("quota admission")
	}
	r.move(a, "running")
	r.move(a, "delivery")
	r.release(a, "running")
	if r.count() != 2 {
		t.Fatal("requeue released running token")
	}
	r.release(a, "")
	r.release(a, "")
	if r.count() != 1 || !r.admit(b, "pool") {
		t.Fatal("exactly-once release")
	}
}
func TestPendingDeliveryDuplicateOwnership(t *testing.T) {
	e := &Engine{residents: newResidentSet(SchedulerLimits{Global: 10, Site: 10, Stage: 10})}
	c := e.deliveryState()
	if !c.AcquireForStage(1, "a:s") || c.AcquireForStage(1, "a:s") {
		t.Fatal("duplicate pending ownership")
	}
	c.Release(1)
	if !c.AcquireForStage(1, "a:s") {
		t.Fatal("released pending cannot be admitted")
	}
}
func TestDelayDedupAndDistantOffload(t *testing.T) {
	e := &Engine{residents: newResidentSet(SchedulerLimits{Global: 3, Site: 3, Stage: 3, DelayWindow: time.Minute})}
	task := &Task{ID: 1, Site: "a", Stage: "s"}
	at := time.Now().Add(time.Second)
	for i := 0; i < 100; i++ {
		e.enqueueDelayed(at, task, models.CrawlerTask{})
	}
	e.enqueueDelayed(at, &Task{ID: 1, Site: "a", Stage: "s"}, models.CrawlerTask{})
	if e.delays.Len() != 1 || e.SchedulerResidentCount() != 1 {
		t.Fatalf("dedup: %d/%d", e.delays.Len(), e.SchedulerResidentCount())
	}
	e.enqueueDelayed(time.Now().Add(time.Hour), &Task{ID: 2, Site: "a", Stage: "s"}, models.CrawlerTask{})
	if e.delays.Len() != 1 || e.SchedulerResidentCount() != 1 {
		t.Fatal("far task retained")
	}
}

type resourceTestFetcher struct {
	run func(context.Context, *Task, *Engine) error
}

func (f resourceTestFetcher) GetStage() string { return "detail" }
func (f resourceTestFetcher) FetchHandler(c context.Context, t *Task, e *Engine) error {
	return f.run(c, t, e)
}
func TestSchedulerMySQLRecursiveDurableRefillWithOneToken(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 1, Site: 1, Stage: 1, RefillBatch: 4}); err != nil {
		t.Fatal(err)
	}

	var completed atomic.Int64
	info := e.stages["synthetic:detail"]
	info.config.WorkerCount = 1
	info.config.MaxAttempts = 1
	info.fetcher = resourceTestFetcher{run: func(ctx context.Context, task *Task, e *Engine) error {
		if task.Meta["root"] == "yes" {
			for i := 0; i < 20; i++ {
				if err := e.SubmitTask(&Task{Site: "synthetic", Stage: "detail", URL: fmt.Sprintf("https://example.invalid/child/%d", i), Meta: map[string]string{"child": "preserved"}}); err != nil {
					return err
				}
				if e.SchedulerResidentCount() > 1 {
					return fmt.Errorf("resident overflow")
				}
			}
		} else if task.Meta["child"] != "preserved" {
			return fmt.Errorf("lost metadata")
		}
		completed.Add(1)
		return nil
	}}
	e.pools.Get("synthetic:detail").Start(e.ctx, e.stageHandler(info))
	e.schedulerStarted = true
	e.startPendingRecovery()
	if err := e.SubmitTask(&Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/root", Meta: map[string]string{"root": "yes"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for completed.Load() != 21 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if completed.Load() != 21 {
		t.Fatalf("executed %d/21", completed.Load())
	}
	var n int64
	for time.Now().Before(deadline) {
		db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusSuccess).Count(&n)
		if n == 21 && e.SchedulerResidentCount() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if n != 21 || e.SchedulerResidentCount() != 0 {
		t.Fatalf("terminal/resident %d/%d", n, e.SchedulerResidentCount())
	}
}

func TestSchedulerMySQLStaleRoundAndDelayedBatchPointer(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 1, Site: 1, Stage: 1}); err != nil {
		t.Fatal(err)
	}
	task := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/delayed-batch", Repeatable: true, Delay: time.Second}
	if err := e.SubmitTasks([]*Task{task}); err != nil {
		t.Fatal(err)
	}
	if err := e.SubmitTasks([]*Task{task}); err != nil {
		t.Fatal(err)
	}
	var delayedRow models.CrawlerTask
	if err := db.First(&delayedRow, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if delayedRow.NotBefore == nil {
		t.Fatal("delay deadline not persisted")
	}
	e.enqueueDelayed(*delayedRow.NotBefore, task, delayedRow)
	e.enqueueDelayed(*delayedRow.NotBefore, task, delayedRow)
	if e.delays.Len() != 1 || e.SchedulerResidentCount() != 1 {
		t.Fatalf("same pointer lost token: heap/resident %d/%d", e.delays.Len(), e.SchedulerResidentCount())
	}
	// A new engine observes the old round, then an operator advances it while
	// keeping a nonnil deadline already in the past.
	old := makeEngine()
	var row models.CrawlerTask
	if err := db.First(&row, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	copy := old.taskFromRecord(&row)
	old.residents.admit(copy, "running")
	past := gorm.Expr("DATE_SUB(UTC_TIMESTAMP(3), INTERVAL 1 SECOND)")
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Updates(map[string]any{"reprocess": row.Reprocess + 1, "not_before": past}).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := old.claimTask(copy)
	if err != nil || claimed {
		t.Fatalf("stale claim %v %v", claimed, err)
	}
	if old.delays.Len() != 0 {
		t.Fatal("stale round self-perpetuates in delay")
	}
	old.residents.release(copy, "running")
	old.schedulerStarted = true
	if _, err := old.recoverPendingStage("synthetic:detail"); err != nil {
		t.Fatal(err)
	}
	if old.SchedulerResidentCount() != 1 {
		db.First(&row, task.ID)
		t.Fatalf("new round not rediscovered: row=%+v", row)
	}
}
func TestSchedulerMySQLPausedPendingReleasesCapacityForOtherSite(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 1, Site: 1, Stage: 1}); err != nil {
		t.Fatal(err)
	}
	e.setPause(pauseScope{Site: "synthetic"}, pauseState{Source: PauseSourceManual})
	if err := e.SubmitTask(&Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/paused"}); err != nil {
		t.Fatal(err)
	}
	e.stages["other:detail"] = &stageInfo{stage: "detail", config: StageConfig{Site: "other", WorkerCount: 1, QueueSize: 8}}
	e.pools.Set("other:detail", workerpool.NewWorkerPool[*Task](1, 8))
	active := &Task{Site: "other", Stage: "detail", URL: "https://example.invalid/active"}
	if err := e.SubmitTask(active); err != nil {
		t.Fatal(err)
	}
	e.schedulerStarted = true
	if _, err := e.recoverPendingStage("other:detail"); err != nil {
		t.Fatal(err)
	}
	main, urgent := e.pools.Get("other:detail").QueueDepths()
	if main+urgent != 1 || e.SchedulerResidentCount() != 1 {
		t.Fatalf("paused site pinned budget: queue=%d resident=%d", main+urgent, e.SchedulerResidentCount())
	}
	var pending int64
	db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusPending).Count(&pending)
	if pending != 2 {
		t.Fatal("durable task lost", pending)
	}
}

func TestSchedulerMySQLWaitingFlowDoesNotPinLastToken(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	e.cfg.TaskFlow.Enabled = true
	if err := db.AutoMigrate(&models.TaskFlow{}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 1, Site: 1, Stage: 1}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := "resource-flow"
	policy := FlowPolicy{Stages: map[string]string{"detail": "items"}, Lanes: map[string]FlowLane{"items": {"depth", 1}}}
	if err := e.CreateTaskFlow(ctx, id, "synthetic", policy); err != nil {
		t.Fatal(err)
	}
	service, _ := e.taskFlows()
	a, err := service.register(ctx, id, 0, (&Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/flow/a"}).toModel())
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.register(ctx, id, 0, (&Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/flow/b"}).toModel())
	if err != nil {
		t.Fatal(err)
	}
	waiting := e.taskFromRecord(&b)
	e.residents.admit(waiting, "running")
	if claimed, err := e.claimFlowTask(ctx, waiting); err != nil || claimed {
		t.Fatalf("waiting claim %v %v", claimed, err)
	}
	if e.SchedulerResidentCount() != 0 {
		t.Fatal("waiting flow pinned last token")
	}
	e.schedulerStarted = true
	if _, err := e.recoverPendingStage("synthetic:detail"); err != nil {
		t.Fatal(err)
	}
	if !e.residents.containsID(int(a.ID)) {
		t.Fatal("eligible flow prerequisite not admitted")
	}
}

func TestSchedulerMySQLPauseReleasesQueuedCapacityAndKeepsFutureDurable(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed=%v", delayed), func(t *testing.T) {
			db, makeEngine := compensationMySQL(t)
			e := makeEngine()
			if err := e.SetSchedulerLimits(SchedulerLimits{Global: 1, Site: 1, Stage: 1}); err != nil {
				t.Fatal(err)
			}
			var called atomic.Int64
			info := e.stages["synthetic:detail"]
			info.config.WorkerCount = 1
			info.fetcher = resourceTestFetcher{run: func(context.Context, *Task, *Engine) error { called.Add(1); return nil }}
			held := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/held"}
			if delayed {
				held.Delay = 30 * time.Second
			}
			if err := e.SubmitTask(held); err != nil {
				t.Fatal(err)
			}
			if !delayed {
				if _, err := e.recoverPendingStage("synthetic:detail"); err != nil {
					t.Fatal(err)
				}
			}
			wantResident := 1
			if delayed {
				wantResident = 0
			}
			if e.SchedulerResidentCount() != wantResident {
				t.Fatalf("initial resident=%d, want %d", e.SchedulerResidentCount(), wantResident)
			}
			e.setPause(pauseScope{Site: "synthetic"}, pauseState{Source: PauseSourceManual})
			e.stages["other:detail"] = &stageInfo{stage: "detail", config: StageConfig{Site: "other", WorkerCount: 1, QueueSize: 8}}
			e.pools.Set("other:detail", workerpool.NewWorkerPool[*Task](1, 8))
			if err := e.SubmitTask(&Task{Site: "other", Stage: "detail", URL: "https://example.invalid/other"}); err != nil {
				t.Fatal(err)
			}
			pool := e.pools.Get("synthetic:detail")
			pool.SetGate(e.pauseGateFor(pauseScope{Site: "synthetic", Stage: "detail"}))
			pool.Start(e.ctx, e.stageHandler(info))
			e.schedulerStarted = true
			deadline := time.Now().Add(3 * time.Second)
			queued := 0
			for time.Now().Before(deadline) {
				if _, err := e.recoverPendingStage("other:detail"); err != nil {
					t.Fatal(err)
				}
				a, b := e.pools.Get("other:detail").QueueDepths()
				queued = a + b
				if queued == 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if queued != 1 || called.Load() != 0 || e.delays.Len() != 0 || e.SchedulerResidentCount() != 1 {
				t.Fatalf("paused transfer: other=%d handler=%d delay=%d resident=%d", queued, called.Load(), e.delays.Len(), e.SchedulerResidentCount())
			}
			var row models.CrawlerTask
			if err := db.First(&row, held.ID).Error; err != nil || row.Status != models.TaskStatusPending {
				t.Fatal("paused durable task changed", err, row.Status)
			}
		})
	}
}

func TestSchedulerMySQLStopCountsDurableBeforeApply(t *testing.T) {
	_, makeEngine := compensationMySQL(t)
	e := makeEngine()
	task := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/before-apply", Delay: time.Hour}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if e.schedulerStarted || e.SchedulerResidentCount() != 0 || e.delays.Len() != 0 {
		t.Fatal("test must have only durable work before Apply")
	}
	drained, stats := e.Stop(time.Second)
	if drained || stats["synthetic:detail"].Queued != 1 {
		t.Fatalf("unreported startup durable work: drained=%v stats=%v", drained, stats)
	}
}

func TestSchedulerMySQLDelayRecoveryInitializesFirstRoundOnlyOnClaim(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	task := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/first-delayed", Delay: time.Hour, Repeatable: true}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if e.SchedulerResidentCount() != 0 {
		t.Fatal("far task not offloaded")
	}
	if err := db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).Update("not_before", gorm.Expr("DATE_SUB(UTC_TIMESTAMP(3),INTERVAL 1 SECOND)")).Error; err != nil {
		t.Fatal(err)
	}
	e.schedulerStarted = true
	if _, err := e.recoverPendingStage("synthetic:detail"); err != nil {
		t.Fatal(err)
	}
	var row models.CrawlerTask
	if err := db.First(&row, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Repeat != 0 || e.SchedulerResidentCount() != 1 {
		t.Fatalf("pending delivery changed round: repeat=%d resident=%d", row.Repeat, e.SchedulerResidentCount())
	}
	claimed, err := e.claimTask(e.taskFromRecord(&row))
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	if err := db.First(&row, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Repeat != 1 {
		t.Fatalf("claimed first round=%d", row.Repeat)
	}
}
