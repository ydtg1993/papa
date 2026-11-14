package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/ydtg1993/papa/v4/models"
)

func stageContractFixture(t *testing.T) (*Engine, *fakeTaskDB) {
	t.Helper()
	f := newFakeTaskDB()
	e := newTestEngine(t, f)
	for _, st := range []struct {
		name  string
		entry bool
		next  []string
	}{{"root", true, []string{"middle"}}, {"middle", false, []string{"leaf"}}, {"leaf", false, nil}, {"peer", true, nil}} {
		if err := e.AddStage(st.name, StageConfig{Site: "s", Entry: st.entry, Downstream: st.next, WorkerCount: 1, QueueSize: 1}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.sealStageSubmissions(); err != nil {
		t.Fatal(err)
	}
	e.closeRootSubmissions()
	return e, f
}
func TestStageSubmissionRejectsIllegalEdgesAndIdentityBeforeStorage(t *testing.T) {
	for _, name := range []string{"same", "reverse", "skip", "cross-site", "forged-parent", "copied-parent", "mutated-parent", "missing-context", "expired", "cancelled-context", "without-cancel"} {
		t.Run(name, func(t *testing.T) {
			e, f := stageContractFixture(t)
			parent := &Task{ID: 7, Site: "s", Stage: "middle"}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, end := e.bindTaskExecution(base, parent)
			defer end()
			child := &Task{Site: "s", Stage: "leaf", URL: "https://example.invalid/child"}
			switch name {
			case "same":
				child.Stage = "middle"
			case "reverse":
				child.Stage = "root"
			case "skip":
				parent.Stage = "root"
			case "cross-site":
				child.Site = "other"
			case "forged-parent":
				child.PID = 99
			case "copied-parent":
				v := *parent
				parent = &v
			case "mutated-parent":
				parent.ID = 99
			case "missing-context":
				ctx = context.Background()
			case "expired":
				end()
			case "cancelled-context":
				cancel()
			case "without-cancel":
				cancel()
				ctx = context.WithoutCancel(ctx)
			}
			err := e.SubmitChildren(ctx, parent, child)
			if err == nil {
				t.Fatal("invalid execution or edge accepted")
			}
			if f.written() != "" {
				t.Fatal("invalid edge touched storage", f.written())
			}
			if child.PID != 0 && name != "forged-parent" {
				t.Fatal("invalid batch mutated child")
			}
		})
	}
}
func TestRuntimeRootFallbackAndStageReplacementAreClosed(t *testing.T) {
	e, f := stageContractFixture(t)
	root := &Task{Site: "s", Stage: "root", URL: "https://example.invalid/new-root"}
	for _, run := range []func() error{func() error { return e.SubmitTask(root) }, func() error { return e.SubmitTasks([]*Task{root}) }, func() error { return e.SubmitFlowTask(context.Background(), "flow", 0, root) }, func() error {
		return e.AddStage("root", StageConfig{Site: "s", Entry: true, Downstream: []string{"root"}, WorkerCount: 1, QueueSize: 1}, nil, nil)
	}, e.sealStageSubmissions} {
		if err := run(); !errors.Is(err, ErrTaskFlow) {
			t.Fatalf("bypass err=%v", err)
		}
	}
	if f.written() != "" {
		t.Fatal("root fallback wrote to storage")
	}
}
func TestStageSubmissionBatchValidatesBeforeMutatingOrWriting(t *testing.T) {
	e, f := stageContractFixture(t)
	parent := &Task{ID: 7, Site: "s", Stage: "middle"}
	ctx, end := e.bindTaskExecution(context.Background(), parent)
	defer end()
	good := &Task{Site: "s", Stage: "leaf", URL: "https://example.invalid/good"}
	bad := &Task{Site: "s", Stage: "middle", URL: "https://example.invalid/bad"}
	if err := e.SubmitChildren(ctx, parent, good, bad); !errors.Is(err, ErrTaskFlow) {
		t.Fatal(err)
	}
	if good.PID != 0 || f.written() != "" {
		t.Fatal("invalid batch partially accepted")
	}
}
func TestStageSubmissionRechecksPersistedIDOwnership(t *testing.T) {
	e, f := stageContractFixture(t)
	parent := &Task{ID: 7, Site: "s", Stage: "middle"}
	ctx, end := e.bindTaskExecution(context.Background(), parent)
	defer end()
	f.row["site"] = "s"
	f.row["stage"] = "root"
	f.row["status"] = int64(models.TaskStatusPending)
	// The identity lookup must miss, while the explicit ID points at another stage.
	f.onQuery = func(n int) { f.noRows = n == 1 }
	child := &Task{ID: 7, Site: "s", Stage: "leaf", URL: "https://example.invalid/forged", Meta: map[string]string{"changed": "true"}}
	if err := e.SubmitChildren(ctx, parent, child); !errors.Is(err, ErrTaskOwnershipConflict) {
		t.Fatalf("ownership mismatch=%v", err)
	}
	if f.written() != "" {
		t.Fatal("persisted identity overwritten", f.written())
	}
	if e.dedupCache.Get(child.Unique()) {
		t.Fatal("rejected identity poisoned dedup cache")
	}
}
