package engine

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ydtg1993/papa/v4/core"
)

var (
	ErrRuleScheduleInvalid     = errors.New("rule schedule: invalid configuration or control task")
	ErrRuleScheduleCorrupt     = errors.New("rule schedule: corrupt state or orphaned batch")
	ErrRuleScheduleConflict    = errors.New("rule schedule: control state changed")
	ErrRuleScheduleUnconfirmed = errors.New("rule schedule: state commit unconfirmed; retry the same controller")
)

type RuleScheduleConfig struct {
	Stage    string
	Entry    *RuleFetcher
	Interval time.Duration
}

// RuleSchedule serializes complete batches through a separate repeatable stage.
// It requires the rules migration and an ordinary, nontransactional MySQL Engine.
// Existing tasks in each batch keep their immutable rule/publication bindings.
type RuleSchedule struct{ config RuleScheduleConfig }

type RuleScheduleStatus struct {
	TaskID                                      uint
	Revision                                    uint64
	Sequence                                    uint64
	BatchID                                     string
	RootID                                      uint
	ReleaseID                                   string
	ReleaseGeneration                           uint64
	Total, Pending, Processing, Success, Failed int64
	Complete                                    bool
}

func NewRuleSchedule(cfg RuleScheduleConfig) (*RuleSchedule, error) {
	if cfg.Entry == nil || !scheduleText(cfg.Stage, 128) || cfg.Stage == cfg.Entry.config.Stage || cfg.Interval < 10*time.Second || cfg.Interval%time.Second != 0 || cfg.Interval/time.Second > math.MaxInt32 {
		return nil, ErrRuleScheduleInvalid
	}
	if _, exists := cfg.Entry.config.Bindings[cfg.Stage]; exists {
		return nil, ErrRuleScheduleInvalid
	}
	for _, link := range cfg.Entry.config.Links {
		if link.Stage == cfg.Stage {
			return nil, ErrRuleScheduleInvalid
		}
	}
	return &RuleSchedule{config: cfg}, nil
}

func (s *RuleSchedule) GetStage() string { return s.config.Stage }

// NewTask constructs a stable controller without reading Catalog.Current.
// Its immutable metadata contains request identity, never transient credentials.
func (s *RuleSchedule) NewTask(request RuleRequest, meta map[string]string) (*Task, error) {
	if s == nil {
		return nil, ErrRuleScheduleInvalid
	}
	spec := scheduleSpec{SchemaVersion: 1, Request: request, Account: s.config.Entry.config.AccountID, Meta: meta}
	return s.specTask(spec)
}

// Status reports only durable tasks. No state row means the controller has not
// allocated its first batch. A missing root is never a completed batch.
func (s *RuleSchedule) Status(ctx context.Context, e *Engine, taskID uint) (RuleScheduleStatus, error) {
	if s == nil || ctx == nil || taskID == 0 {
		return RuleScheduleStatus{}, ErrRuleScheduleInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	db, err := scheduleDB(e)
	if err != nil {
		return RuleScheduleStatus{}, err
	}
	control, state, err := s.readSchedule(ctx, db, taskID)
	if err != nil {
		return RuleScheduleStatus{}, err
	}
	return s.batchStatus(ctx, db, control, state)
}

func (s *RuleSchedule) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	if e == nil {
		return core.WrapNoRetry(ErrRuleScheduleInvalid)
	}
	return s.runSchedule(ctx, task, e, e.SubmitTask)
}

func (s *RuleSchedule) runSchedule(ctx context.Context, task *Task, e *Engine, submit func(*Task) error) error {
	if s == nil || ctx == nil || task == nil || task.ID <= 0 {
		return core.WrapNoRetry(ErrRuleScheduleInvalid)
	}
	version, claimed := taskRunVersionFrom(ctx)
	if !claimed {
		return core.WrapNoRetry(ErrRuleScheduleInvalid)
	}
	db, err := scheduleDB(e)
	if err != nil {
		return core.WrapNoRetry(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	control, state, err := s.readSchedule(bounded, db, uint(task.ID))
	if err != nil {
		return err
	}
	if task.Site != control.Site || task.Stage != control.Stage || task.URL != control.URL || ruleJSON(task.Meta) != ruleJSON(control.specTask.Meta) || task.IdempotencyKey != "" {
		return core.WrapNoRetry(ErrRuleScheduleInvalid)
	}
	if !control.processing() || version.Repeat != control.Repeat || version.Reprocess != control.Reprocess {
		return ErrRuleScheduleConflict
	}
	if state != nil {
		status, err := s.batchStatus(bounded, db, control, state)
		if err != nil {
			return err
		}
		if !status.Complete || state.payload.Tick == scheduleTick(control.LastRepeatAt) {
			if status.RootID == 0 {
				return submit(state.payload.Root.task())
			}
			return nil // active/failed batches await their existing task recovery.
		}
	}
	sequence := uint64(1)
	if state != nil {
		if state.payload.Sequence == math.MaxUint64 {
			return core.WrapNoRetry(ErrRuleScheduleCorrupt)
		}
		sequence = state.payload.Sequence + 1
	}
	// Catalog and rule validation happen before taking database locks.
	next, err := s.prepareBatch(bounded, control, sequence)
	if err != nil {
		return err
	}
	if err := s.commitBatch(bounded, db, control, state, next); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return submit(next.Root.task())
}
