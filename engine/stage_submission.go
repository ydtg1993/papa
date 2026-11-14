package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ydtg1993/papa/v4/engine/internal/delivery"
	"github.com/ydtg1993/papa/v4/internal/stageflow"
)

// ErrTaskFlow identifies a submission outside the declared downward stage graph.
var ErrTaskFlow = errors.New("task submission violates stage flow")

// stageSubmissionPolicy owns the immutable stage contracts and root-admission
// phase. It never retains Engine, task payload queues or storage dependencies.
type stageSubmissionPolicy struct {
	mu          sync.RWMutex
	frozen      bool
	rootsClosed bool
	graphs      map[string]*stageflow.Graph
}

func compileStageContracts(stages map[string]*stageInfo) (map[string]*stageflow.Graph, error) {
	sites := map[string]map[string]stageflow.Node{}
	for _, s := range stages {
		if sites[s.config.Site] == nil {
			sites[s.config.Site] = map[string]stageflow.Node{}
		}
		sites[s.config.Site][s.stage] = stageflow.Node{Entry: s.config.Entry, Downstream: s.config.Downstream}
	}
	graphs := map[string]*stageflow.Graph{}
	for site, nodes := range sites {
		g, err := stageflow.Compile(nodes)
		if err != nil {
			return nil, fmt.Errorf("%w: site %s: %v", ErrTaskFlow, site, err)
		}
		graphs[site] = g
	}
	return graphs, nil
}

func (e *Engine) sealStageSubmissions() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := &e.stageSubmissions
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.frozen {
		return fmt.Errorf("%w: stages already started", ErrTaskFlow)
	}
	graphs, err := compileStageContracts(e.stages)
	if err != nil {
		return err
	}
	p.graphs = graphs
	p.frozen = true
	return nil
}
func (e *Engine) closeRootSubmissions() {
	p := &e.stageSubmissions
	p.mu.Lock()
	p.rootsClosed = true
	p.mu.Unlock()
}

// validateRootSubmissions is intentionally unavailable once workers can run.
// Dropping the handler context or switching to SubmitTask cannot create a root.
func (e *Engine) validateRootSubmissions(tasks []*Task) error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	p := &e.stageSubmissions
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.rootsClosed {
		return fmt.Errorf("%w: startup entry admission closed; use SubmitChildren with handler context", ErrTaskFlow)
	}
	graphs := p.graphs
	if !p.frozen {
		var err error
		graphs, err = compileStageContracts(e.stages)
		if err != nil {
			return err
		}
	}
	for _, t := range tasks {
		if t == nil || t.PID != 0 {
			return fmt.Errorf("%w: root task must not have a parent", ErrTaskFlow)
		}
		g := graphs[t.Site]
		if g == nil || !g.Root(t.Stage) {
			return fmt.Errorf("%w: %s:%s is not an entry root", ErrTaskFlow, t.Site, t.Stage)
		}
	}
	return nil
}

type taskExecutionKey struct{}

// taskExecutionPermit snapshots the source identity before invoking business.
// Its lock makes callback completion wait for already accepted synchronous work;
// closing prevents future use even when a caller retained the original context.
type taskExecutionPermit struct {
	sourceContext     context.Context
	mu                sync.RWMutex
	active            bool
	policy            *stageSubmissionPolicy
	parent            *Task
	id                int
	site, stage, flow string
}

func (e *Engine) bindTaskExecution(ctx context.Context, t *Task) (context.Context, func()) {
	p := &taskExecutionPermit{sourceContext: ctx, active: true, policy: &e.stageSubmissions, parent: t, id: t.ID, site: t.Site, stage: t.Stage, flow: t.FlowID}
	return context.WithValue(ctx, taskExecutionKey{}, p), func() { p.mu.Lock(); p.active = false; p.mu.Unlock() }
}
func (e *Engine) acquireTaskExecution(ctx context.Context, parent *Task) (*taskExecutionPermit, func(), error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("%w: missing handler context", ErrTaskFlow)
	}
	p, _ := ctx.Value(taskExecutionKey{}).(*taskExecutionPermit)
	if p == nil {
		return nil, nil, fmt.Errorf("%w: active handler context required", ErrTaskFlow)
	}
	p.mu.RLock()
	if !p.active || p.policy != &e.stageSubmissions || p.parent != parent || parent == nil || p.id <= 0 || parent.ID != p.id || parent.Site != p.site || parent.Stage != p.stage || parent.FlowID != p.flow {
		p.mu.RUnlock()
		return nil, nil, fmt.Errorf("%w: expired or altered execution identity", ErrTaskFlow)
	}
	if err := p.sourceContext.Err(); err != nil {
		p.mu.RUnlock()
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		p.mu.RUnlock()
		return nil, nil, err
	}
	return p, p.mu.RUnlock, nil
}
func (e *Engine) validateChildSubmission(p *taskExecutionPermit, t *Task) error {
	if t == nil || t.Site != p.site || (t.PID != 0 && t.PID != p.id) {
		return fmt.Errorf("%w: child site or parent mismatch", ErrTaskFlow)
	}
	policy := &e.stageSubmissions
	policy.mu.RLock()
	g := policy.graphs[p.site]
	policy.mu.RUnlock()
	if g == nil || !g.Allows(p.stage, t.Stage) {
		return fmt.Errorf("%w: undeclared or non-downward edge %s:%s -> %s", ErrTaskFlow, p.site, p.stage, t.Stage)
	}
	return e.validateTask(t)
}

// SubmitChildren is the only ordinary runtime producer. It validates the entire
// batch before writing and derives PID from an unmodified active handler task.
func (e *Engine) SubmitChildren(ctx context.Context, parent *Task, children ...*Task) error {
	p, release, err := e.acquireTaskExecution(ctx, parent)
	if err != nil {
		return err
	}
	defer release()
	if p.flow != "" {
		return fmt.Errorf("%w: TaskFlow children must use SubmitFlowTask", ErrTaskFlow)
	}
	for _, t := range children {
		if err := e.validateChildSubmission(p, t); err != nil {
			return err
		}
		if t.FlowID != "" {
			return ErrFlowInvalid
		}
	}
	for _, t := range children {
		t.PID = p.id
	}
	merged, cancel := delivery.Merge(ctx, p.sourceContext)
	defer cancel()
	return e.submitTasksContext(merged, children)
}

func (e *Engine) SubmitTask(task *Task) error {
	if err := e.beginTaskAdmission(); err != nil {
		return err
	}
	defer e.endTaskAdmission()
	if err := e.validateRootSubmissions([]*Task{task}); err != nil {
		return err
	}
	return e.submitTask(task)
}
func (e *Engine) SubmitTasks(tasks []*Task) error {
	if err := e.beginTaskAdmission(); err != nil {
		return err
	}
	defer e.endTaskAdmission()
	if err := e.validateRootSubmissions(tasks); err != nil {
		return err
	}
	return e.submitTasksContext(e.queueContext(), tasks)
}
