package agentstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func exerciseStore(t *testing.T, store Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	id := prefix + "-lifecycle"
	r, err := store.Start(ctx, Run{ID: id, SnapshotJSON: `{"target":"articles"}`})
	if err != nil || r.Revision != 1 || r.Status != Running || r.CreatedAt.IsZero() || !r.CreatedAt.Equal(r.UpdatedAt) {
		t.Fatalf("start: %+v %v", r, err)
	}
	if _, err := store.Start(ctx, Run{ID: id}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate start: %v", err)
	}
	r, step, err := store.BeginStep(ctx, id, r.Revision, Step{Kind: "model"}, `{"calls":1}`)
	if err != nil || r.Revision != 2 || step.Sequence != 1 || step.Status != StepPending || step.StartedAt.IsZero() {
		t.Fatalf("begin: %+v %+v %v", r, step, err)
	}
	if _, _, err := store.BeginStep(ctx, id, r.Revision, Step{Kind: "fetch"}, `{"calls":2}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("second pending step: %v", err)
	}
	if _, err := store.Finish(ctx, id, r.Revision, Failed, `{}`, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("finish pending: %v", err)
	}
	if _, err := store.UpdateUsage(ctx, id, 1, `{"calls":90}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale usage: %v", err)
	}
	r, err = store.UpdateUsage(ctx, id, r.Revision, `{"calls":1,"requests":2}`)
	if err != nil || r.Revision != 3 {
		t.Fatalf("pending usage: %+v %v", r, err)
	}
	before := r
	step.Status, step.OutputJSON = StepSucceeded, `{bad`
	if _, err := store.EndStep(ctx, id, r.Revision, step, `{"calls":90}`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid end: %v", err)
	}
	r, err = store.Get(ctx, id)
	if err != nil || !reflect.DeepEqual(r, before) {
		t.Fatalf("failed end mutated run: %+v %v", r, err)
	}
	step.OutputJSON = `{"candidate_id":"v1"}`
	r, err = store.EndStep(ctx, id, r.Revision, step, `{"calls":1}`)
	if err != nil || r.Revision != 4 {
		t.Fatalf("end: %+v %v", r, err)
	}
	if _, err := store.EndStep(ctx, id, r.Revision, step, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate end: %v", err)
	}
	r, step, err = store.BeginStep(ctx, id, r.Revision, Step{Kind: "fetch"}, r.UsageJSON)
	if err != nil || step.Sequence != 2 {
		t.Fatalf("sequence: %+v %v", step, err)
	}
	step.Status = StepFailed
	r, err = store.EndStep(ctx, id, r.Revision, step, r.UsageJSON)
	if err != nil {
		t.Fatal(err)
	}
	r, err = store.Finish(ctx, id, r.Revision, NeedsFixtures, `{"candidate_id":"v1"}`, r.UsageJSON)
	if err != nil || r.Status != NeedsFixtures || r.Revision != 7 || r.FinishedAt == nil {
		t.Fatalf("finish: %+v %v", r, err)
	}
	if _, err := store.UpdateUsage(ctx, id, r.Revision, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal update: %v", err)
	}
	if _, err := store.Interrupt(ctx, id, r.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal interrupt: %v", err)
	}
	*r.FinishedAt = time.Time{}
	r, err = store.Get(ctx, id)
	if err != nil || r.FinishedAt.IsZero() {
		t.Fatalf("run not detached: %+v %v", r, err)
	}
	steps, err := store.Steps(ctx, id, 0)
	if err != nil || len(steps) != 2 || steps[0].Status != StepSucceeded || steps[1].Status != StepFailed {
		t.Fatalf("steps: %+v %v", steps, err)
	}
	*steps[0].FinishedAt = time.Time{}
	steps[0].InputJSON = `{"mutated":true}`
	steps, err = store.Steps(ctx, id, 1)
	if err != nil || len(steps) != 1 || steps[0].FinishedAt.IsZero() || steps[0].InputJSON != "{}" {
		t.Fatalf("steps not detached: %+v %v", steps, err)
	}
	if _, err := store.Steps(ctx, id, MaxStepsLimit+1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unbounded read: %v", err)
	}
}

func exerciseInterruptAndCancel(t *testing.T, store Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	r, err := store.Start(ctx, Run{ID: prefix + "-interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err = store.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{"reserved":1}`)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Interrupt(canceled, r.ID, r.Revision); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel interrupt: %v", err)
	}
	if _, err := store.UpdateUsage(canceled, r.ID, r.Revision, `{}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel update: %v", err)
	}
	if _, _, err := store.BeginStep(canceled, r.ID, r.Revision, Step{Kind: "model"}, `{}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel begin: %v", err)
	}
	if _, err := store.Get(canceled, r.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel get: %v", err)
	}
	if _, err := store.Start(canceled, Run{ID: prefix + "-canceled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel start: %v", err)
	}
	if _, err := store.Interrupt(ctx, r.ID, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale interrupt: %v", err)
	}
	r, err = store.Interrupt(ctx, r.ID, r.Revision)
	if err != nil || r.Revision != 3 || r.Status != Interrupted || r.UsageJSON != `{"reserved":1}` {
		t.Fatalf("interrupt: %+v %v", r, err)
	}
	steps, err := store.Steps(ctx, r.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Status != StepUnknown || steps[0].FinishedAt == nil {
		t.Fatalf("unknown pending action: %+v %v", steps, err)
	}
	if _, _, err := store.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed interrupted action: %v", err)
	}
}

func exerciseCAS(t *testing.T, store Store, prefix string) {
	t.Helper()
	r, err := store.Start(context.Background(), Run{ID: prefix + "-cas"})
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.UpdateUsage(context.Background(), r.ID, r.Revision, `{"reserved":1}`)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("CAS: %v", err)
			}
		}()
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatalf("CAS winners = %d", successes.Load())
	}
	after, err := store.Get(context.Background(), r.ID)
	if err != nil || after.Revision != 2 {
		t.Fatalf("CAS result: %+v %v", after, err)
	}
}

func TestMemoryStoreLifecycleAndAtomicity(t *testing.T) { exerciseStore(t, NewMemoryStore(), "memory") }
func TestMemoryStoreInterruptAndCancel(t *testing.T) {
	exerciseInterruptAndCancel(t, NewMemoryStore(), "memory")
}
func TestMemoryStoreCAS(t *testing.T) { exerciseCAS(t, NewMemoryStore(), "memory") }

func TestMetadataBoundsAndInvalidInputs(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	for _, r := range []Run{{ID: "bad id"}, {ID: "x", SnapshotJSON: `{"x":`}, {ID: "x", UsageJSON: strings.Repeat("x", MaxJSONBytes+1)}, {ID: "x", SnapshotJSON: `"` + strings.Repeat("x", MaxSnapshotBytes) + `"`}, {ID: "x", Status: Failed}, {ID: "x", Revision: 1}, {ID: "x", ResultJSON: `{"fake":true}`}, {ID: "x", CreatedAt: time.Now()}} {
		if _, err := s.Start(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid start accepted: %v", err)
		}
	}
	r, err := s.Start(ctx, Run{ID: "case"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, Run{ID: "CASE"}); err != nil {
		t.Fatalf("case-sensitive ID: %v", err)
	}
	for _, step := range []Step{{Kind: "fetch", InputJSON: `[broken`}, {Kind: "fetch", InputJSON: `"` + strings.Repeat("x", MaxJSONBytes) + `"`}, {Kind: "fetch", Status: StepSucceeded}, {Kind: "fetch", RunID: "wrong"}, {Kind: ""}} {
		if _, _, err := s.BeginStep(ctx, r.ID, r.Revision, step, `{}`); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid step accepted: %v", err)
		}
	}
	if _, err := s.Finish(ctx, r.ID, r.Revision, Interrupted, `{}`, `{}`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("interrupt bypass: %v", err)
	}
	if _, err := s.Finish(ctx, r.ID, r.Revision, "new-state", `{}`, `{}`); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status: %v", err)
	}
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Steps(ctx, "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	after, _ := s.Get(ctx, r.ID)
	if after.Revision != 1 {
		t.Fatalf("invalid input mutated run: %+v", after)
	}
}
