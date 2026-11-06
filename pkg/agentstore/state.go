package agentstore

import (
	"math"
	"time"
)

type runState struct {
	run               Run
	sequence, pending uint64
}

type operation struct {
	kind                  string
	step                  Step
	usage, status, result string
}

// change is shared by both stores; persistence applies its entire result or none.
func change(state runState, previous *Step, revision uint64, op operation, now time.Time) (runState, *Step, error) {
	if err := writable(state.run, revision); err != nil {
		return runState{}, nil, err
	}
	if state.pending != 0 && (previous == nil || previous.Status != StepPending || previous.Sequence != state.pending || previous.RunID != state.run.ID) {
		return runState{}, nil, ErrCorrupt
	}
	var changed *Step
	var err error
	if op.kind != "interrupt" {
		state.run.UsageJSON, err = checkedJSON(op.usage, MaxJSONBytes)
		if err != nil {
			return runState{}, nil, err
		}
	}
	switch op.kind {
	case "begin":
		if state.pending != 0 {
			return runState{}, nil, ErrConflict
		}
		if state.sequence == math.MaxUint64 {
			return runState{}, nil, ErrInvalid
		}
		s, err := prepareStep(state.run.ID, op.step)
		if err != nil {
			return runState{}, nil, err
		}
		if s.Sequence != 0 && s.Sequence != state.sequence+1 {
			return runState{}, nil, ErrConflict
		}
		state.sequence++
		state.pending = state.sequence
		s.Sequence, s.StartedAt = state.sequence, now
		changed = &s
	case "end":
		if previous == nil || state.pending == 0 || op.step.Sequence != state.pending {
			return runState{}, nil, ErrConflict
		}
		if (op.step.RunID != "" && op.step.RunID != state.run.ID) || (op.step.Kind != "" && op.step.Kind != previous.Kind) || (op.step.InputJSON != "" && op.step.InputJSON != previous.InputJSON) || (op.step.Status != StepSucceeded && op.step.Status != StepFailed) {
			return runState{}, nil, ErrInvalid
		}
		s := copyStep(*previous)
		s.OutputJSON, err = checkedJSON(op.step.OutputJSON, MaxJSONBytes)
		if err != nil {
			return runState{}, nil, err
		}
		s.Status, s.FinishedAt = op.step.Status, &now
		changed, state.pending = &s, 0
	case "usage":
	case "finish":
		if state.pending != 0 {
			return runState{}, nil, ErrConflict
		}
		if !terminal(op.status) || op.status == Interrupted {
			return runState{}, nil, ErrInvalid
		}
		state.run.ResultJSON, err = checkedJSON(op.result, MaxJSONBytes)
		if err != nil {
			return runState{}, nil, err
		}
		state.run.Status, state.run.FinishedAt = op.status, &now
	case "interrupt":
		if previous != nil {
			s := copyStep(*previous)
			s.Status, s.FinishedAt = StepUnknown, &now
			changed, state.pending = &s, 0
		}
		state.run.Status, state.run.FinishedAt = Interrupted, &now
	default:
		return runState{}, nil, ErrInvalid
	}
	state.run.Revision++
	state.run.UpdatedAt = now
	return state, changed, nil
}
