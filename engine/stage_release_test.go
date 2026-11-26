package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
)

func TestStageReleaseRearmsPendingRoundAfterTerminal(t *testing.T) {
	for _, automaticRepeat := range []bool{false, true} {
		name := "admin_retry"
		if automaticRepeat {
			name = "automatic_repeat"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["repeat"], f.row["reprocess"] = int64(2), int64(3)
			f.row["compensation_version"] = int64(0)
			if automaticRepeat {
				f.row["repeatable"] = int64(models.RepeatableYes)
				f.row["next_repeat_at"] = time.Now().Add(-time.Minute)
			}
			f.rows = []fakeRow{f.row}
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := queueEngine(t, f, pool)
			if err := e.SetSchedulerLimits(SchedulerLimits{Global: 2, Site: 2, Stage: 2}); err != nil {
				t.Fatal(err)
			}
			mirrorStageReleaseWrites(t, e, f)
			const key = "stub:stub"
			fetcher := &recordingFetcher{stage: "stub"}
			info := e.stages[key]
			info.fetcher = fetcher
			info.config.MaxAttempts = 1
			info.config.WorkerCount = 1
			info.config.Delay = config.DurationRange{Min: time.Hour, Max: time.Hour}
			finished := make(chan struct{}, 2)
			// Synchronous test observer: the terminal write has completed before
			// the next round is created. The hour-long delay keeps the old owner.
			e.events = stagePoolEventFunc(func(ev events.Event) {
				if ev.Type == events.TypeTaskFinished {
					finished <- struct{}{}
				}
			})
			row, err := e.loadTask(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			old := e.taskFromRecord(row)
			if !e.deliveryState().AcquireForStage(old.ID, key) || !e.residents.admit(old, "pool") {
				t.Fatal("old round admission failed")
			}
			oldCtx, cancelOld := context.WithCancel(context.Background())
			defer cancelOld()
			oldDone := make(chan error, 1)
			go func() { oldDone <- e.stageHandler(info)(oldCtx, old) }()
			select {
			case <-finished:
			case err := <-oldDone:
				t.Fatalf("old handler exited before terminal success: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("old handler did not persist success")
			}

			row, err = e.loadTask(context.Background(), 7)
			if err != nil || row.Status != models.TaskStatusSuccess {
				t.Fatalf("terminal row = %+v, err = %v", row, err)
			}
			wantRepeat, wantReprocess := row.Repeat, row.Reprocess
			if automaticRepeat {
				// The automatic ticker calls this same due-only queue entry point.
				if n, err := e.RepollRepeatableTasks(); err != nil || n != 1 {
					t.Fatalf("automatic repeat = %d, %v", n, err)
				}
				wantRepeat++
			} else {
				if err := e.RetryTaskVersioned(context.Background(), row.ID, row.Reprocess, row.CompensationVersion); err != nil {
					t.Fatal(err)
				}
				wantReprocess++
			}
			requirePendingNotification(t, e, key)
			if _, err := e.recoverPendingStage(key); err != nil {
				t.Fatal(err)
			}
			if _, _, ended := e.deliveryState().Scan(key); !ended || queued(t, pool) != 0 {
				t.Fatal("initial scan must skip the old owner and exhaust the stage")
			}
			if !e.residents.available("stub", key) {
				t.Fatal("fixture needs spare capacity to expose the ownership race")
			}

			// Observe actual stageHandler cleanup at the existing capacity
			// callback. Delivery ownership must still fence admission here:
			// publishing its recheck wake earlier lets a scan skip the resident
			// and finish before that resident disappears, losing the new round.
			ownerAtResidentRelease := make(chan bool, 2)
			capacityReleased := e.residents.onRelease
			e.residents.onRelease = func() {
				ownerAtResidentRelease <- e.deliveryState().Owned(old.ID)
				capacityReleased()
			}
			cancelOld()
			waitStageReleaseHandler(t, oldDone)
			if !<-ownerAtResidentRelease {
				t.Error("delivery recheck was published before resident release; a pending scan can exhaust while the old resident still blocks it")
			}
			if e.deliveryState().Owned(old.ID) || e.residents.containsID(old.ID) {
				t.Fatal("old handler retained scheduler ownership")
			}
			// No external Wake/Restart is allowed: only the old handler's
			// production cleanup can restart the already exhausted scan.
			requirePendingNotification(t, e, key)
			if _, err := e.recoverPendingStage(key); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 1 || e.recoveredCount.Load() != 1 {
				t.Fatalf("new round delivery: queued=%d recovered=%d", queued(t, pool), e.recoveredCount.Load())
			}
			if _, err := e.recoverPendingStage(key); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 1 {
				t.Fatal("exhausted scan duplicated the new round")
			}

			info.config.Delay = config.DurationRange{}
			newDone := make(chan error, 1)
			handler := e.stageHandler(info)
			pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
				err := handler(ctx, task)
				newDone <- err
				return err
			})
			t.Cleanup(func() { pool.Stop(time.Second) })
			waitStageReleaseHandler(t, newDone)
			if drained, _ := pool.Stop(time.Second); !drained {
				t.Fatal("new round did not drain")
			}
			row, err = e.loadTask(context.Background(), 7)
			if err != nil || row.Status != models.TaskStatusSuccess || row.Repeat != wantRepeat || row.Reprocess != wantReprocess {
				t.Fatalf("new round result = %+v, err = %v; want success repeat=%d reprocess=%d", row, err, wantRepeat, wantReprocess)
			}
			if len(fetcher.calls()) != 2 || e.recoveredCount.Load() != 1 || e.SchedulerResidentCount() != 0 || e.deliveryState().Owned(old.ID) {
				t.Fatalf("round executed more than once or leaked ownership: calls=%d recovered=%d residents=%d", len(fetcher.calls()), e.recoveredCount.Load(), e.SchedulerResidentCount())
			}
			if increments := strings.Count(f.written(), "`repeat` + 1") + strings.Count(f.written(), "reprocess + 1"); increments != 1 {
				t.Fatalf("recovery advanced the durable round again: increments=%d", increments)
			}
			select {
			case <-e.deliveryState().Notifications(key):
				t.Fatal("finished recovery generated an extra pending wake")
			default:
			}
		})
	}
}

func waitStageReleaseHandler(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stage handler did not finish cleanup")
	}
}

// The existing fake does not evaluate UPDATEs. Mirror only status and additive
// counters so production handlers/admin/repeat paths share their confirmed row.
// SQL predicates are covered by taskstore tests; this is a scheduler regression.
func mirrorStageReleaseWrites(t *testing.T, e *Engine, f *fakeTaskDB) {
	t.Helper()
	err := e.db.Callback().Update().After("gorm:update").Register("test:stage-release-row", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if !ok || tx.Error != nil || tx.RowsAffected != 1 {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if status, ok := values["status"].(models.TaskStatus); ok {
			f.row["status"] = int64(status)
		}
		for column, expression := range map[string]string{"repeat": "`repeat` + 1", "reprocess": "reprocess + 1", "compensation_version": "compensation_version + 1"} {
			if strings.Contains(tx.Statement.SQL.String(), expression) {
				value, _ := asInt64(f.row[column])
				f.row[column] = value + 1
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
