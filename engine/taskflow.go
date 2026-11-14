package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/taskflow"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// ErrTaskFlow identifies an invalid task origin or undeclared downstream edge.
var ErrTaskFlow = errors.New("task submission violates stage flow")

// validateStageFlows also protects low-level callers that opt into StrictFlow.
func (e *Engine) validateStageFlows() error {
	graphs := map[string]map[string][]string{}
	strict := map[string]bool{}
	for _, info := range e.stages {
		site := info.config.Site
		if graphs[site] == nil {
			graphs[site] = map[string][]string{}
		}
		graphs[site][info.stage] = info.config.Downstream
		strict[site] = strict[site] || info.config.StrictFlow
	}
	for site, edges := range graphs {
		if strict[site] {
			if err := taskflow.Validate(site, edges); err != nil {
				return fmt.Errorf("%w: %v", ErrTaskFlow, err)
			}
		}
	}
	return nil
}

type executionContextKey struct{}
type entrySubmissionContextKey struct{}

// SubmitEntryTask is the explicit startup/control-plane entry submission API.
// Handler-derived tasks must use SubmitChildren; entry admission is not a
// fallback for rejected same-stage or undeclared child submissions.
func (e *Engine) SubmitEntryTask(task *Task) error {
	return e.SubmitEntryTaskContext(context.Background(), task)
}

// SubmitEntryTaskContext accepts only parentless tasks in an Entry stage.
// Passing a handler context does not grant permission to submit a new root.
func (e *Engine) SubmitEntryTaskContext(ctx context.Context, task *Task) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil entry context", ErrTaskFlow)
	}
	if _, ok := ctx.Value(executionContextKey{}).(*executionPermit); ok {
		return fmt.Errorf("%w: handlers must use SubmitChildren, not entry admission", ErrTaskFlow)
	}
	return e.submitAccepted(context.WithValue(ctx, entrySubmissionContextKey{}, e), []*Task{task}, false, false)
}

// executionPermit is issued only by the worker, for one handler invocation.
// Captured identity is immutable: changing public Task fields cannot change it.
type executionPermit struct {
	engine      *Engine
	task        *Task
	id          int
	site, stage string
	active      atomic.Bool
}

func (e *Engine) bindTaskExecution(ctx context.Context, task *Task) (context.Context, func()) {
	p := &executionPermit{engine: e, task: task, id: task.ID, site: task.Site, stage: task.Stage}
	p.active.Store(true)
	ctx, cancel := context.WithCancel(ctx)
	return context.WithValue(ctx, executionContextKey{}, p), func() { p.active.Store(false); cancel() }
}

func (e *Engine) executionPermit(ctx context.Context, parent *Task) (*executionPermit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: missing handler context", ErrTaskFlow)
	}
	p, _ := ctx.Value(executionContextKey{}).(*executionPermit)
	if p == nil || p.engine != e || !p.active.Load() || p.task != parent || parent == nil ||
		p.id <= 0 || parent.ID != p.id || parent.Site != p.site || parent.Stage != p.stage {
		return nil, fmt.Errorf("%w: requires the active handler context and original parent task", ErrTaskFlow)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

func (e *Engine) validateChild(p *executionPermit, task *Task) error {
	if task == nil {
		return fmt.Errorf("%w: nil child", ErrTaskFlow)
	}
	if task.Site != p.site {
		return fmt.Errorf("%w: cross-site child %q -> %q", ErrTaskFlow, p.site, task.Site)
	}
	source := e.stages[core.StageKey(p.site, p.stage)]
	target := e.stages[core.StageKey(task.Site, task.Stage)]
	if source == nil || target == nil {
		return fmt.Errorf("%w: unregistered source or target stage", ErrTaskFlow)
	}
	if task.Stage == p.stage {
		return fmt.Errorf("%w: same-stage child %s:%s", ErrTaskFlow, p.site, p.stage)
	}
	if (source.config.StrictFlow || target.config.StrictFlow) && !slices.Contains(source.config.Downstream, task.Stage) {
		return fmt.Errorf("%w: %s:%s does not allow downstream %q", ErrTaskFlow, p.site, p.stage, task.Stage)
	}
	if task.PID != 0 && task.PID != p.id {
		return fmt.Errorf("%w: child parent ID does not match active handler", ErrTaskFlow)
	}
	return nil
}

// SubmitChildren submits only declared downstream tasks from an active handler.
// The entire batch is checked before changing any task or writing to storage.
// Use the handler's ctx and task unchanged; never replace ctx with Background.
func (e *Engine) SubmitChildren(ctx context.Context, parent *Task, children ...*Task) error {
	p, err := e.executionPermit(ctx, parent)
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := e.validateChild(p, child); err != nil {
			return err
		}
		if err := e.validateTask(child); err != nil {
			return err
		}
	}
	for _, child := range children {
		child.PID = p.id
	}
	return e.SubmitTasksContext(ctx, children)
}

// validateSubmissionContext runs before dedup/insert on every public submission.
// Low-level AddStage callers can retain legacy root semantics (StrictFlow=false);
// all App.RegisterSites stages enable StrictFlow and deny undeclared root work.
func (e *Engine) validateSubmissionContext(ctx context.Context, tasks []*Task) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrTaskFlow)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p, ok := ctx.Value(executionContextKey{}).(*executionPermit); ok {
		if _, err := e.executionPermit(ctx, p.task); err != nil {
			return err
		}
		for _, task := range tasks {
			if err := e.validateChild(p, task); err != nil {
				return err
			}
			if task.PID != p.id {
				return fmt.Errorf("%w: use SubmitChildren to attach parent identity", ErrTaskFlow)
			}
		}
		return nil
	}
	for _, task := range tasks {
		if task == nil {
			return fmt.Errorf("%w: nil task", ErrTaskFlow)
		}
		info := e.stages[core.StageKey(task.Site, task.Stage)]
		if info != nil && info.config.StrictFlow {
			if ctx.Value(entrySubmissionContextKey{}) != e {
				return fmt.Errorf("%w: use SubmitEntryTask for entry work or SubmitChildren for handler work", ErrTaskFlow)
			}
			if task.PID != 0 || !info.config.Entry {
				return fmt.Errorf("%w: root submissions require Entry stage and no parent; handler children must use SubmitChildren", ErrTaskFlow)
			}
		}
	}
	return nil
}

// validatePersistedTask checks the durable identity returned by either dedup
// index. A caller-supplied idempotency key can collide with another stage/site;
// validating only the submitted task would then authorize the wrong row.
func (e *Engine) validatePersistedTask(task *Task, record *models.CrawlerTask) error {
	if task == nil || record == nil {
		return fmt.Errorf("%w: missing submitted or persisted task", ErrTaskFlow)
	}
	info := e.stages[core.StageKey(task.Site, task.Stage)]
	if info != nil && info.config.StrictFlow && (record.Site != task.Site || record.Stage != task.Stage) {
		return fmt.Errorf("%w: dedup resolved %s:%s to persisted task %d in %s:%s",
			ErrTaskFlow, task.Site, task.Stage, record.ID, record.Site, record.Stage)
	}
	return nil
}

// rejectTaskAdminContext prevents accidentally using the trusted control-plane
// operation from a handler. Expired permits are rejected as well. Deliberately
// discarding context is outside the trusted application's correctness boundary.
func rejectTaskAdminContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil administrative context", ErrTaskFlow)
	}
	if _, ok := ctx.Value(executionContextKey{}).(*executionPermit); ok {
		return fmt.Errorf("%w: handlers must use SubmitChildren, not administrative admission", ErrTaskFlow)
	}
	return ctx.Err()
}

func taskMetaScope(db *gorm.DB, p *executionPermit) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("id = ? AND site = ? AND stage = ? AND status = ?", p.id, p.site, p.stage, models.TaskStatusProcessing)
}

// SaveTaskMeta checkpoints the active task's business metadata. It never changes
// status, counters or identity. Call only after the associated child submissions
// have been accepted durably. A failure must stop advancing the business cursor.
func (e *Engine) SaveTaskMeta(ctx context.Context, task *Task) error {
	p, err := e.executionPermit(ctx, task)
	if err != nil {
		return err
	}
	if e.db == nil {
		return errors.New("save task meta: database is unavailable")
	}
	res := taskMetaScope(e.db.WithContext(ctx), p).Updates(map[string]any{"meta": metaToJSON(task.Meta)})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// MySQL may report zero for identical values; distinguish that from an
		// operator deleting/settling the task while the handler was running.
		var n int64
		if err := taskMetaScope(e.db.WithContext(ctx), p).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("save task meta: task %d is no longer processing", p.id)
		}
	}
	return nil
}
