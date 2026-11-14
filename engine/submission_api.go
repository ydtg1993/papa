package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v3/internal/delivery"
	"time"
)

var ErrEngineStopped = errors.New("engine is not accepting tasks")

func (e *Engine) SubmitTask(task *Task) error { return e.SubmitTaskContext(context.Background(), task) }
func (e *Engine) SubmitTasks(tasks []*Task) error {
	return e.SubmitTasksContext(context.Background(), tasks)
}
func (e *Engine) SubmitTaskContext(ctx context.Context, task *Task) error {
	return e.submitAccepted(ctx, []*Task{task}, false, false)
}
func (e *Engine) SubmitTasksContext(ctx context.Context, tasks []*Task) error {
	return e.submitAccepted(ctx, tasks, false, true)
}

// SubmitAdministrativeTaskContext is an explicit control-plane operation.
// HTTP handlers must authorize their caller before using it; fetchers use SubmitChildren.
func (e *Engine) SubmitAdministrativeTaskContext(ctx context.Context, task *Task) error {
	if err := rejectTaskAdminContext(ctx); err != nil {
		return err
	}
	return e.submitAccepted(ctx, []*Task{task}, true, false)
}

func (e *Engine) submitAccepted(ctx context.Context, tasks []*Task, admin, batch bool) error {
	if ctx == nil {
		return errors.New("nil submission context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !admin {
		if err := e.validateSubmissionContext(ctx, tasks); err != nil {
			return err
		}
	}
	if !e.deliveryState().Begin() {
		return ErrEngineStopped
	}
	defer e.deliveryState().End()
	ctx, done := delivery.Merge(ctx, e.lifecycleContext())
	defer done()
	if err := ctx.Err(); err != nil {
		return err
	}
	copies := make([]*Task, len(tasks))
	for i, t := range tasks {
		if t == nil {
			return errors.New("nil task")
		}
		v := *t
		v.submitContext = ctx
		if v.NotBefore.IsZero() && v.Delay > 0 {
			v.NotBefore = time.Now().Add(v.Delay)
			v.Delay = 0
		}
		if !v.NotBefore.IsZero() {
			v.NotBefore = v.NotBefore.Add(time.Millisecond - time.Nanosecond).Truncate(time.Millisecond)
		}
		copies[i] = &v
	}
	defer func() {
		for i, t := range copies {
			tasks[i].ID = t.ID
			tasks[i].Repeat = t.Repeat
		}
	}()
	if batch {
		return e.submitTasks(copies)
	}
	return e.submitTask(copies[0])
}
