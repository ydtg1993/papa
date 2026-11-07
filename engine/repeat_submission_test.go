package engine

import (
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
)

// A second public submission is another delivery of the existing pending
// round, not a new polling round. Use distinct Task objects so an object-local
// repeatCounted flag cannot hide duplicate counting across callers.
func TestConcurrentPublicSubmissionsKeepPendingRepeat(t *testing.T) {
	for _, entry := range []string{"SubmitTask", "SubmitTasks", "mixed"} {
		t.Run(entry, func(t *testing.T) {
			const callers = 8
			f := newFakeTaskDB()
			f.row["repeat"] = int64(12)
			f.row["repeatable"] = int64(models.RepeatableYes)
			pool := workerpool.NewWorkerPool[*Task](1, 32, 1)
			e := submitEngine(t, f, pool)
			// No worker or drain loop runs. The pause gate retains one delivery
			// in the spill list, keeping the same pending round throughout.
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
				if task.ID != 7 || task.Repeat != 12 {
					t.Errorf("caller %d: task ID=%d Repeat=%d, want ID=7 Repeat=12 for the existing pending round", i, task.ID, task.Repeat)
				}
			}
			if queued(t, pool) != 0 || e.spillBacklog() != 1 {
				t.Errorf("paused stage: queued=%d spilled=%d, want 0 queued and 1 retained delivery", queued(t, pool), e.spillBacklog())
			}
			// fakeTaskDB synchronizes query/exec recording, but does NOT execute
			// SQL, evaluate WHERE, or enforce transactions. This checks emitted
			// SQL and caller-visible counters; real MySQL must confirm the stored
			// counter under concurrency. No mutable fake hooks are used here.
			if sql := f.written(); strings.Contains(sql, "`repeat` + 1") {
				t.Errorf("resubmitting one pending round emitted repeat increments:\n%s", sql)
			}
		})
	}
}
