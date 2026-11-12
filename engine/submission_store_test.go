package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/pkg/account"
)

type submissionReferenceProbe struct {
	active bool
	calls  int
}

func (p *submissionReferenceProbe) ProtectReferences(_ context.Context, keys []account.PoolKey, insert func() error) error {
	if len(keys) != 2 || keys[0].Name != "pool" || keys[1].Name != "pool" {
		return errors.New("wrong references")
	}
	p.calls++
	p.active = true
	defer func() { p.active = false }()
	return insert()
}

func TestSubmissionStoreProtectsEntireBatchFallback(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	f.failNext = 1
	guard := &submissionReferenceProbe{}
	f.onExec = func(int) {
		if !guard.active {
			t.Error("insert escaped reference protection")
		}
	}
	s := submissionStore{db: openFakeTaskDB(t, f), references: guard, logger: quietLogger()}
	tasks := []*Task{{Site: "stub", Stage: "stub", URL: "https://example.invalid/a", AccountPool: "pool"}, {Site: "stub", Stage: "stub", URL: "https://example.invalid/b", AccountPool: "pool"}}
	conflicts, failed, err := s.insertTasks(tasks)
	if err != nil || len(conflicts) != 0 || len(failed) != 0 || guard.calls != 1 || guard.active {
		t.Fatalf("fallback: conflicts=%v failed=%v guard=%+v err=%v", conflicts, failed, guard, err)
	}
	if writes := strings.Count(f.written(), "INSERT INTO"); writes != 3 {
		t.Fatalf("want batch and two protected retries, got %d", writes)
	}
}

func TestSubmissionStoreTransfersUnreadCommittedTask(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	f.onQuery = func(n int) {
		if n == 2 {
			f.failQueries = 1
		}
	}
	s := submissionStore{db: openFakeTaskDB(t, f), dedupCache: newDedupCache(16), logger: quietLogger()}
	task := &Task{Site: "stub", Stage: "stub", URL: "https://example.invalid/a"}
	plan, err := s.prepare(task)
	if err == nil || task.ID == 0 || len(plan.deliveries) != 1 || !plan.deliveries[0].unread || plan.deliveries[0].task != task {
		t.Fatalf("committed task lost: id=%d plan=%+v err=%v", task.ID, plan, err)
	}
	if s.dedupCache.Get(task.Unique()) {
		t.Fatal("failed reload prematurely cached")
	}
}

func TestSubmissionStoreRejectsOwnershipBeforeBatchWrites(t *testing.T) {
	f := newFakeTaskDB()
	s := submissionStore{db: openFakeTaskDB(t, f), dedupCache: newDedupCache(16), logger: quietLogger()}
	a := &Task{Site: "stub", Stage: "stub", URL: "https://example.invalid/a", AccountPool: "one"}
	b := *a
	b.AccountPool = "two"
	plan, err := s.prepareBatch([]*Task{a, &b}, func(submissionDelivery) { t.Fatal("ownership conflict transferred a task") })
	if !errors.Is(err, ErrTaskOwnershipConflict) || len(plan.deliveries) != 0 || f.written() != "" || f.readSQL() != "" {
		t.Fatalf("ownership conflict touched persistence: %+v %v", plan, err)
	}
}

func TestSubmitTasksDeliversBeforeNextRowReload(t *testing.T) {
	f := newFakeTaskDB()
	blocked, release := make(chan struct{}), make(chan struct{})
	reloads := 0
	f.onQuery = func(n int) {
		f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?")
		if f.noRows {
			return
		}
		reloads++
		if reloads == 2 {
			close(blocked)
			<-release
		}
	}
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	done := make(chan error, 1)
	go func() {
		done <- e.SubmitTasks([]*Task{
			{Site: "stub", Stage: "stub", URL: "https://example.invalid/first"},
			{Site: "stub", Stage: "stub", URL: "https://example.invalid/second"},
		})
	}()
	t.Cleanup(func() {
		close(release)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("batch did not resume")
		}
	})
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("second reload was not reached")
	}
	if got := queued(t, pool); got != 1 {
		t.Fatalf("later row reload postponed earlier committed row: queued=%d", got)
	}
}

type submissionLogWriter func([]byte) (int, error)

func (w submissionLogWriter) Write(p []byte) (int, error) { return w(p) }

func TestSubmitTaskRetainsCommittedTaskBeforeErrorLogging(t *testing.T) {
	for _, panicLog := range []bool{false, true} {
		name := "normal"
		if panicLog {
			name = "panicking_sink"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows = true
			f.onQuery = func(n int) {
				if n == 2 {
					f.failQueries = 1
				}
			}
			e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
			logged := false
			e.loggerSet.Engine.SetOutput(submissionLogWriter(func(p []byte) (int, error) {
				logged = true
				if got := e.spillBacklog(); got != 1 {
					t.Errorf("committed task was not retained before logger call: spill=%d", got)
				}
				if panicLog {
					panic("test log sink")
				}
				return len(p), nil
			}))
			task := &Task{Site: "stub", Stage: "stub", URL: "https://example.invalid/committed"}
			var recovered any
			var err error
			func() {
				defer func() { recovered = recover() }()
				err = e.SubmitTask(task)
			}()
			if !logged || task.ID == 0 || e.spillBacklog() != 1 {
				t.Fatalf("lost committed task: logged=%v id=%d spill=%d", logged, task.ID, e.spillBacklog())
			}
			if panicLog && recovered != "test log sink" {
				t.Fatalf("panic changed: %v", recovered)
			}
			if !panicLog && (err == nil || recovered != nil) {
				t.Fatalf("reload error changed: %v panic=%v", err, recovered)
			}
		})
	}
}

func TestSubmissionBatchBackfillsDuplicatesWhenHandoffPanics(t *testing.T) {
	f := newFakeTaskDB()
	f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }
	s := submissionStore{db: openFakeTaskDB(t, f), dedupCache: newDedupCache(16), logger: quietLogger()}
	first := &Task{Site: "stub", Stage: "stub", URL: "https://example.invalid/first", Repeatable: true}
	duplicate := *first
	later := &Task{Site: "stub", Stage: "stub", URL: "https://example.invalid/later"}
	transfers := 0
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = s.prepareBatch([]*Task{first, &duplicate, later}, func(delivery submissionDelivery) {
			transfers++
			delivery.task.Repeat, delivery.task.repeatCounted = 1, true
			panic("handoff failed")
		})
	}()
	if recovered != "handoff failed" || transfers != 1 {
		t.Fatalf("handoff panic was swallowed or replayed: %v transfers=%d", recovered, transfers)
	}
	if first.ID == 0 || later.ID == 0 {
		t.Fatal("batch was not persisted before handoff")
	}
	if duplicate.ID != first.ID || duplicate.Repeat != first.Repeat || !duplicate.repeatCounted {
		t.Fatalf("panic skipped duplicate backfill: first=%+v duplicate=%+v", first, duplicate)
	}
}
