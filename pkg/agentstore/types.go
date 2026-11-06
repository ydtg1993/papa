// Package agentstore persists bounded audit metadata before external actions.
// It never migrates a database or automatically resumes/intercepts a running job.
// Callers must supply sanitized references and metadata, not bodies, prompts,
// credentials or full model replies. JSON validation is not secret redaction.
package agentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

const (
	Running          = "running"
	AwaitingReview   = "awaiting_review"
	NeedsFixtures    = "needs_fixtures"
	Exhausted        = "exhausted"
	Failed           = "failed"
	Canceled         = "canceled"
	Interrupted      = "interrupted"
	StepPending      = "pending"
	StepSucceeded    = "succeeded"
	StepFailed       = "failed"
	StepUnknown      = "unknown"
	MaxSnapshotBytes = 1 << 20
	MaxJSONBytes     = 64 << 10
	MaxStepsLimit    = 4096
)

var (
	ErrInvalid  = errors.New("agentstore: invalid metadata")
	ErrConflict = errors.New("agentstore: revision or state conflict")
	ErrNotFound = errors.New("agentstore: run not found")
	ErrCorrupt  = errors.New("agentstore: corrupt audit record")
)

type Run struct {
	ID, Status                          string
	Revision                            uint64
	SnapshotJSON, UsageJSON, ResultJSON string
	CreatedAt, UpdatedAt                time.Time
	FinishedAt                          *time.Time
}

type Step struct {
	RunID                               string
	Sequence                            uint64
	Kind, InputJSON, OutputJSON, Status string
	StartedAt                           time.Time
	FinishedAt                          *time.Time
}

// Store mutations return authoritative state only after a successful commit.
// A commit error, including an unknown outcome, never authorizes an external
// call. Recover by explicitly interrupting after the old worker has stopped.
type Store interface {
	Start(context.Context, Run) (Run, error)
	BeginStep(context.Context, string, uint64, Step, string) (Run, Step, error)
	EndStep(context.Context, string, uint64, Step, string) (Run, error)
	UpdateUsage(context.Context, string, uint64, string) (Run, error)
	Finish(context.Context, string, uint64, string, string, string) (Run, error)
	Get(context.Context, string) (Run, error)
	Steps(context.Context, string, int) ([]Step, error)
	// Interrupt requires the host to have established that the previous worker
	// has ended. Pending actions become unknown and are never blindly replayed.
	Interrupt(context.Context, string, uint64) (Run, error)
}

func identifier(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

func checkedJSON(s string, limit int) (string, error) {
	if s == "" {
		s = "{}"
	}
	if len(s) > limit || !utf8.ValidString(s) || !json.Valid([]byte(s)) {
		return "", ErrInvalid
	}
	return s, nil
}

func terminal(status string) bool {
	switch status {
	case AwaitingReview, NeedsFixtures, Exhausted, Failed, Canceled, Interrupted:
		return true
	}
	return false
}

func prepareStart(r Run) (Run, error) {
	if !identifier(r.ID, 128) || (r.Status != "" && r.Status != Running) || r.Revision != 0 || !r.CreatedAt.IsZero() || !r.UpdatedAt.IsZero() || r.FinishedAt != nil {
		return Run{}, ErrInvalid
	}
	var err error
	if r.SnapshotJSON, err = checkedJSON(r.SnapshotJSON, MaxSnapshotBytes); err != nil {
		return Run{}, err
	}
	if r.UsageJSON, err = checkedJSON(r.UsageJSON, MaxJSONBytes); err != nil {
		return Run{}, err
	}
	if r.ResultJSON != "" && r.ResultJSON != "{}" {
		return Run{}, ErrInvalid
	}
	r.ResultJSON, r.Status, r.Revision = "{}", Running, 1
	return r, nil
}

func prepareStep(id string, s Step) (Step, error) {
	if (s.RunID != "" && s.RunID != id) || !identifier(s.Kind, 64) || (s.Status != "" && s.Status != StepPending) || !s.StartedAt.IsZero() || s.FinishedAt != nil || (s.OutputJSON != "" && s.OutputJSON != "{}") {
		return Step{}, ErrInvalid
	}
	var err error
	if s.InputJSON, err = checkedJSON(s.InputJSON, MaxJSONBytes); err != nil {
		return Step{}, err
	}
	s.RunID, s.Status, s.OutputJSON = id, StepPending, "{}"
	return s, nil
}

func mutationKey(ctx context.Context, id string, revision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !identifier(id, 128) || revision == 0 || revision == math.MaxUint64 {
		return ErrInvalid
	}
	return nil
}

func writable(r Run, revision uint64) error {
	if r.Status != Running || r.Revision != revision {
		return ErrConflict
	}
	return nil
}

func copyRun(r Run) Run {
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		r.FinishedAt = &t
	}
	return r
}

func copyStep(s Step) Step {
	if s.FinishedAt != nil {
		t := *s.FinishedAt
		s.FinishedAt = &t
	}
	return s
}

func readKey(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !identifier(id, 128) {
		return ErrInvalid
	}
	return nil
}

func checkedLimit(limit int) (int, error) {
	if limit == 0 {
		return 256, nil
	}
	if limit < 0 || limit > MaxStepsLimit {
		return 0, fmt.Errorf("%w: step limit", ErrInvalid)
	}
	return limit, nil
}

func validateStoredRun(r Run) error {
	if !identifier(r.ID, 128) || r.Revision == 0 || (r.Status != Running && !terminal(r.Status)) || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || (r.Status == Running) != (r.FinishedAt == nil) {
		return ErrCorrupt
	}
	for _, field := range []struct {
		text  string
		limit int
	}{{r.SnapshotJSON, MaxSnapshotBytes}, {r.UsageJSON, MaxJSONBytes}, {r.ResultJSON, MaxJSONBytes}} {
		if field.text == "" {
			return ErrCorrupt
		}
		if _, err := checkedJSON(field.text, field.limit); err != nil {
			return ErrCorrupt
		}
	}
	return nil
}

func validateStoredStep(s Step) error {
	if !identifier(s.RunID, 128) || !identifier(s.Kind, 64) || s.Sequence == 0 || s.StartedAt.IsZero() || (s.Status != StepPending && s.Status != StepSucceeded && s.Status != StepFailed && s.Status != StepUnknown) || (s.Status == StepPending) != (s.FinishedAt == nil) {
		return ErrCorrupt
	}
	for _, value := range []string{s.InputJSON, s.OutputJSON} {
		if value == "" {
			return ErrCorrupt
		}
		if _, err := checkedJSON(value, MaxJSONBytes); err != nil {
			return ErrCorrupt
		}
	}
	return nil
}
