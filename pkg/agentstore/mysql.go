package agentstore

import (
	"context"
	"errors"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MySQLStore owns its transactions. New performs no DDL or implicit recovery.
type MySQLStore struct{ db *gorm.DB }

var _ Store = (*MySQLStore)(nil)

func New(db *gorm.DB) (*MySQLStore, error) {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "mysql" || db.DryRun || db.Error != nil {
		return nil, ErrInvalid
	}
	if _, ok := db.ConnPool.(gorm.TxCommitter); ok {
		return nil, ErrInvalid
	}
	if db.Statement != nil {
		if _, ok := db.Statement.ConnPool.(gorm.TxCommitter); ok {
			return nil, ErrInvalid
		}
	}
	// NewDB removes inherited WHERE/table clauses; the caller's context is not
	// retained because each operation has its own cancellation/deadline contract.
	return &MySQLStore{db: db.Session(&gorm.Session{NewDB: true, Initialized: true, Context: context.Background()})}, nil
}

func dbError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	var e *mysqldriver.MySQLError
	if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &e) && e.Number == 1062) {
		return ErrConflict
	}
	return err
}

func checkedWrite(result *gorm.DB) error {
	if result.Error != nil {
		return dbError(result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func serverNow(db *gorm.DB) (time.Time, error) {
	var now time.Time
	err := db.Raw("SELECT CURRENT_TIMESTAMP(6)").Row().Scan(&now)
	return now, err
}

func publicRun(r models.AgentRun) Run {
	return Run{ID: r.ID, Status: r.Status, Revision: r.Revision, SnapshotJSON: r.SnapshotJSON, UsageJSON: r.UsageJSON, ResultJSON: r.ResultJSON, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, FinishedAt: r.FinishedAt}
}

func publicStep(s models.AgentStep) Step {
	return Step{RunID: s.RunID, Sequence: s.Sequence, Kind: s.Kind, InputJSON: s.InputJSON, OutputJSON: s.OutputJSON, Status: s.Status, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt}
}

func stepRow(s Step) models.AgentStep {
	return models.AgentStep{RunID: s.RunID, Sequence: s.Sequence, Kind: s.Kind, InputJSON: s.InputJSON, OutputJSON: s.OutputJSON, Status: s.Status, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt}
}

func (s *MySQLStore) Start(ctx context.Context, run Run) (Run, error) {
	run, err := prepareStart(run)
	if err != nil {
		return Run{}, err
	}
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := serverNow(tx)
		if err != nil {
			return err
		}
		run.CreatedAt, run.UpdatedAt = now, now
		row := models.AgentRun{ID: run.ID, Status: run.Status, Revision: run.Revision, SnapshotJSON: run.SnapshotJSON, UsageJSON: run.UsageJSON, ResultJSON: run.ResultJSON, CreatedAt: now, UpdatedAt: now}
		return checkedWrite(tx.Create(&row))
	})
	if err != nil {
		return Run{}, dbError(err)
	}
	return run, nil
}

func (s *MySQLStore) mutate(ctx context.Context, id string, revision uint64, op operation) (Run, Step, error) {
	if err := mutationKey(ctx, id, revision); err != nil {
		return Run{}, Step{}, err
	}
	var out Run
	var stepOut Step
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.AgentRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&row).Error; err != nil {
			return dbError(err)
		}
		state := runState{run: publicRun(row), sequence: row.NextSequence, pending: row.PendingSequence}
		if err := validateStoredRun(state.run); err != nil {
			return err
		}
		if state.pending > state.sequence {
			return ErrCorrupt
		}
		if err := writable(state.run, revision); err != nil {
			return err
		}
		var pending *Step
		if state.pending != 0 {
			var p models.AgentStep
			if err := tx.Where("run_id = ? AND sequence = ?", id, state.pending).Take(&p).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrCorrupt
				}
				return err
			}
			v := publicStep(p)
			if err := validateStoredStep(v); err != nil {
				return err
			}
			pending = &v
		}
		now, err := serverNow(tx)
		if err != nil {
			return err
		}
		next, step, err := change(state, pending, revision, op, now)
		if err != nil {
			return err
		}
		updates := map[string]any{"status": next.run.Status, "revision": next.run.Revision, "usage_json": next.run.UsageJSON, "result_json": next.run.ResultJSON, "next_sequence": next.sequence, "pending_sequence": next.pending, "updated_at": now, "finished_at": next.run.FinishedAt}
		if err := checkedWrite(tx.Model(&models.AgentRun{}).Where("id = ? AND revision = ? AND status = ?", id, revision, Running).Updates(updates)); err != nil {
			return err
		}
		if step != nil {
			if op.kind == "begin" {
				p := stepRow(*step)
				if err := checkedWrite(tx.Create(&p)); err != nil {
					return err
				}
			} else {
				if err := checkedWrite(tx.Model(&models.AgentStep{}).Where("run_id = ? AND sequence = ? AND status = ?", id, step.Sequence, StepPending).Updates(map[string]any{"status": step.Status, "output_json": step.OutputJSON, "finished_at": step.FinishedAt})); err != nil {
					return err
				}
			}
			stepOut = *step
		}
		out = next.run
		return nil
	})
	// Even if COMMIT reached MySQL, an acknowledgement error is failure. A read
	// proving pending reservation exists must never authorize its external call.
	if err != nil {
		return Run{}, Step{}, dbError(err)
	}
	return out, stepOut, nil
}

func (s *MySQLStore) BeginStep(ctx context.Context, id string, revision uint64, step Step, usage string) (Run, Step, error) {
	return s.mutate(ctx, id, revision, operation{kind: "begin", step: step, usage: usage})
}

func (s *MySQLStore) EndStep(ctx context.Context, id string, revision uint64, step Step, usage string) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "end", step: step, usage: usage})
	return r, err
}

func (s *MySQLStore) UpdateUsage(ctx context.Context, id string, revision uint64, usage string) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "usage", usage: usage})
	return r, err
}

func (s *MySQLStore) Finish(ctx context.Context, id string, revision uint64, status, result, usage string) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "finish", status: status, result: result, usage: usage})
	return r, err
}

func (s *MySQLStore) Interrupt(ctx context.Context, id string, revision uint64) (Run, error) {
	r, _, err := s.mutate(ctx, id, revision, operation{kind: "interrupt"})
	return r, err
}

func (s *MySQLStore) Get(ctx context.Context, id string) (Run, error) {
	if err := readKey(ctx, id); err != nil {
		return Run{}, err
	}
	var row models.AgentRun
	if err := s.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
		return Run{}, dbError(err)
	}
	out := publicRun(row)
	if err := validateStoredRun(out); err != nil {
		return Run{}, err
	}
	return out, nil
}

func (s *MySQLStore) Steps(ctx context.Context, id string, limit int) ([]Step, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	limit, err := checkedLimit(limit)
	if err != nil {
		return nil, err
	}
	var rows []models.AgentStep
	if err := s.db.WithContext(ctx).Where("run_id = ?", id).Order("sequence ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, dbError(err)
	}
	out := make([]Step, len(rows))
	for i := range rows {
		out[i] = publicStep(rows[i])
		if err := validateStoredStep(out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
