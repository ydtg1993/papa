package engine

import (
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
)

// A second public submission is another delivery of the existing pending
// round, not a new polling round. Use distinct Task objects so an object-local
func TestConcurrentPublicSubmissionsKeepPendingRepeat(t *testing.T) {
	for _, entry := range []string{"SubmitTask", "SubmitTasks", "mixed"} {
		t.Run(entry, func(t *testing.T) {
			const callers = 8
			f := newFakeTaskDB()
			f.row["repeat"] = int64(12)
			f.row["repeatable"] = int64(models.RepeatableYes)
			pool := workerpool.NewWorkerPool[*Task](1, 32)
			e := submitEngine(t, f, pool)
			// No worker or pending dispatcher runs. All callers leave the same
			// durable pending row; caller-visible Repeat is calibrated on claim.
			if !e.setPause(pauseScope{Site: "stub", Stage: "stub"}, pauseState{Source: PauseSourceManual}) {
				t.Fatal("could not pause test stage")
			}
			tasks := make([]*Task, callers)
			errs := make([]error, callers)
			start := make(chan struct{})
			var ready, done sync.WaitGroup
			ready.Add(callers)
			done.Add(callers)
			for i := range tasks {
				tasks[i] = &Task{Site: "stub", Stage: "stub", URL: "https://example.com", Repeatable: true}
				go func(i int) {
					defer done.Done()
					ready.Done()
					<-start
					if entry == "SubmitTasks" || entry == "mixed" && i%2 != 0 {
						errs[i] = e.SubmitTasks([]*Task{tasks[i]})
					} else {
						errs[i] = e.SubmitTask(tasks[i])
					}
				}(i)
			}
			ready.Wait()
			close(start)
			done.Wait()
			for i, task := range tasks {
				if errs[i] != nil {
					t.Errorf("caller %d: %v", i, errs[i])
				}
				if task.ID != 7 {
					t.Errorf("caller %d: task ID=%d, want persisted identity ID=7", i, task.ID)
				}
			}
			if queued(t, pool) != 0 {
				t.Errorf("paused stage: queued=%d, want durable pending without queued work", queued(t, pool))
			}
			// fakeTaskDB synchronizes query/exec recording, but does NOT execute
			// SQL, evaluate WHERE, or enforce transactions. This checks emitted
			// SQL and caller-visible identity; real MySQL must confirm the stored
			// counter under concurrency. No mutable fake hooks are used here.
			if sql := f.written(); strings.Contains(sql, "`repeat` + 1") {
				t.Errorf("resubmitting one pending round emitted repeat increments:\n%s", sql)
			}
		})
	}
}
