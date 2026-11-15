package engine

import (
	"context"
	"testing"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
)

func TestPendingResetLostAcknowledgementRearmsExhaustedScan(t *testing.T) {
	for _, operation := range []string{"error", "repeat", "manual"} {
		t.Run(operation, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(models.TaskStatusFailed)
			f.row["retry_blocked"] = false
			if operation == "repeat" {
				f.row["status"] = int64(models.TaskStatusSuccess)
				f.row["repeatable"] = int64(models.RepeatableYes)
			}
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := submitEngine(t, f, pool)
			row, err := e.loadTask(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			e.deliveryState().Advance("stub:stub", 0, 7, true)
			// Model a committed UPDATE whose driver response is lost. The fake
			// intentionally does not parse SQL; this hook supplies durable state.
			f.onExec = func(int) {
				f.row["status"] = int64(models.TaskStatusPending)
				f.failNext = 1
			}
			switch operation {
			case "error":
				if e.requeueFailedTaskIn(row) {
					t.Fatal("unconfirmed reset counted as success")
				}
			case "repeat":
				if e.requeueRepeatTaskIn(row) {
					t.Fatal("unconfirmed reset counted as success")
				}
			case "manual":
				if err := e.retryTaskRecord(context.Background(), row, row.Reprocess, row.CompensationVersion); err == nil {
					t.Fatal("reset error hidden")
				}
			}
			requirePendingNotification(t, e, "stub:stub")
			if _, err := e.recoverPendingStage("stub:stub"); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 1 {
				t.Fatal("committed pending task was stranded")
			}
			if len(f.execs) != 1 {
				t.Fatal("additive reset was replayed")
			}
		})
	}
}

func TestInsertLostAcknowledgementRearmsPendingRecovery(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows = true
			f.onExec = func(int) {
				f.noRows = false // INSERT committed; acknowledgement was lost.
				f.failNext = 1
			}
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := submitEngine(t, f, pool)
			e.deliveryState().Advance("stub:stub", 0, 7, true)
			task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com"}
			var err error
			if batch {
				err = e.SubmitTasks([]*Task{task})
			} else {
				err = e.SubmitTask(task)
			}
			if err != nil {
				t.Fatal(err)
			}
			requirePendingNotification(t, e, "stub:stub")
			if _, err := e.recoverPendingStage("stub:stub"); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 1 {
				t.Fatal("confirmed inserted task was stranded")
			}
		})
	}
}

func TestTaskFlowStorageFailureRearmsPendingRecovery(t *testing.T) {
	for _, operation := range []string{"register", "update"} {
		t.Run(operation, func(t *testing.T) {
			f := newFakeTaskDB()
			e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))
			e.cfg.TaskFlow.Enabled = true
			e.deliveryState().Advance("stub:stub", 0, 7, true)
			// This driver rejects transactions. The public contract must rearm
			// on storage errors regardless of whether failure preceded commit.
			var err error
			if operation == "register" {
				err = e.SubmitFlowTask(context.Background(), "run", 0, &Task{Site: "stub", Stage: "stub", URL: "https://example.com"})
			} else {
				err = e.UpdateTaskFlow(context.Background(), "run", 1, FlowPolicy{Stages: map[string]string{"stub": "lane"}, Lanes: map[string]FlowLane{"lane": {Mode: "depth", Parallel: 1}}})
			}
			if err == nil {
				t.Fatal("storage error hidden")
			}
			requirePendingNotification(t, e, "stub:stub")
			if cursor, _, end := e.deliveryState().Scan("stub:stub"); cursor != 0 || end {
				t.Fatal("failed transaction left exhausted cursor")
			}
		})
	}
}

func TestInsertLostAcknowledgementAndLookupFailureStillRearmsRecovery(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows = true
			f.onExec = func(int) { f.noRows = false; f.failNext = 1 }
			f.onQuery = func(n int) {
				if n == 2 {
					f.failQueries = 1
				}
			}
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := submitEngine(t, f, pool)
			e.deliveryState().Advance("stub:stub", 0, 7, true)
			task := &Task{Site: "stub", Stage: "stub", URL: "https://example.com"}
			var err error
			if batch {
				err = e.SubmitTasks([]*Task{task})
			} else {
				err = e.SubmitTask(task)
			}
			if err == nil {
				t.Fatal("unconfirmed insert error hidden")
			}
			requirePendingNotification(t, e, "stub:stub")
			if _, err := e.recoverPendingStage("stub:stub"); err != nil {
				t.Fatal(err)
			}
			if queued(t, pool) != 1 {
				t.Fatal("lookup outage stranded committed insert")
			}
		})
	}
}
