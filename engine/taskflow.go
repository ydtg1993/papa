package engine

import (
	"context"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"strings"
)

// taskFlows snapshots the feature gate; the service owns all flow SQL.
func (e *Engine) taskFlows() (taskFlowService, error) {
	if e == nil || e.cfg == nil || !e.cfg.TaskFlow.Enabled || e.db == nil {
		return taskFlowService{}, ErrFlowInvalid
	}
	return taskFlowService{db: e.db}, nil
}

// CreateTaskFlow creates a bounded run. Existing IDs are never overwritten.
func (e *Engine) CreateTaskFlow(ctx context.Context, id, site string, p FlowPolicy) error {
	if id == "" || len(id) > 64 || strings.TrimSpace(id) != id || site == "" || len(site) > 128 || p.Validate() != nil {
		return ErrFlowInvalid
	}
	for stage := range p.Stages {
		if e.stages[core.StageKey(site, stage)] == nil {
			return ErrStageNotRegistered
		}
	}
	service, err := e.taskFlows()
	if err != nil {
		return err
	}
	return service.create(ctx, id, site, p)
}

// UpdateTaskFlow applies policy changes to future claims and wakes waiting work.
func (e *Engine) UpdateTaskFlow(ctx context.Context, id string, revision uint64, p FlowPolicy) error {
	if p.Validate() != nil {
		return ErrFlowInvalid
	}
	service, err := e.taskFlows()
	if err != nil {
		return err
	}
	err = service.update(ctx, id, revision, p)
	if err == nil {
		e.wakeDrainNow()
	}
	return err
}

// SubmitFlowTask registers lineage atomically before transferring to delivery.
func (e *Engine) SubmitFlowTask(ctx context.Context, id string, parent uint, task *Task) error {
	if task == nil || task.AccountPool != "" || task.Repeatable || task.Delay != 0 || !task.NotBefore.IsZero() {
		return ErrFlowInvalid
	}
	if err := e.validateTask(task); err != nil {
		return err
	}
	service, err := e.taskFlows()
	if err != nil {
		return err
	}
	saved, err := service.register(ctx, id, parent, task.toModel())
	if err != nil {
		return err
	}
	*task = *e.taskFromRecord(&saved)
	if saved.Status == models.TaskStatusPending {
		e.submitPendingTask(task)
	}
	e.wakeDrainNow()
	return nil
}

func (e *Engine) claimFlowTask(ctx context.Context, task *Task) (bool, error) {
	service, err := e.taskFlows()
	if err != nil {
		e.spillTask(task)
		return false, nil
	}
	claimed, waiting, err := service.claim(ctx, task.FlowID, uint(task.ID), task.deliveryVersion)
	if err == nil && waiting {
		if e.residents != nil {
			e.residents.release(task, "")
		} else {
			e.spillTask(task)
		}
	}
	return claimed, err
}

type FlowTaskView struct {
	ID     uint              `json:"id"`
	Stage  string            `json:"stage"`
	Lane   string            `json:"lane"`
	Branch uint              `json:"branch"`
	Status models.TaskStatus `json:"status"`
	Reason string            `json:"reason"`
}
type TaskFlowView struct {
	ID          string         `json:"id"`
	Site        string         `json:"site"`
	Revision    uint64         `json:"revision"`
	Policy      FlowPolicy     `json:"policy"`
	Tasks       []FlowTaskView `json:"tasks"`
	ActiveCount int            `json:"active_count"`
}

func (e *Engine) TaskFlow(ctx context.Context, id string) (TaskFlowView, error) {
	service, err := e.taskFlows()
	if err != nil {
		return TaskFlowView{}, err
	}
	return service.view(ctx, id)
}
