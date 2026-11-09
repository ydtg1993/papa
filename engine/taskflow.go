package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const flowTaskLimit = 10000

func (e *Engine) flowDB() (*gorm.DB, error) {
	if e == nil || e.cfg == nil || !e.cfg.TaskFlow.Enabled || e.db == nil {
		return nil, ErrFlowInvalid
	}
	return e.db, nil
}
func readFlow(db *gorm.DB, id string, lock bool) (models.TaskFlow, FlowPolicy, error) {
	var row models.TaskFlow
	var policy FlowPolicy
	if id == "" || len(id) > 64 {
		return row, policy, ErrFlowInvalid
	}
	if lock {
		db = db.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := db.Where("id = ?", id).Take(&row).Error; err != nil {
		return row, policy, err
	}
	if json.Unmarshal(row.Policy, &policy) != nil || policy.Validate() != nil {
		return row, policy, ErrFlowInvalid
	}
	return row, policy, nil
}

// CreateTaskFlow creates a bounded run, not a publication or a crawl task.
// Callers supply a stable run ID; an existing ID is never overwritten.
func (e *Engine) CreateTaskFlow(ctx context.Context, id, site string, p FlowPolicy) error {
	if id == "" || len(id) > 64 || strings.TrimSpace(id) != id || site == "" || len(site) > 128 || p.Validate() != nil {
		return ErrFlowInvalid
	}
	for stage := range p.Stages {
		if e.stages[core.StageKey(site, stage)] == nil {
			return ErrStageNotRegistered
		}
	}
	db, err := e.flowDB()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(p)
	return db.WithContext(ctx).Create(&models.TaskFlow{ID: id, Site: site, Revision: 1, Policy: b}).Error
}

// UpdateTaskFlow applies to future claims. Stage-to-lane membership is immutable
// because existing branch identities depend on it. Mode/concurrency are mutable.
func (e *Engine) UpdateTaskFlow(ctx context.Context, id string, revision uint64, p FlowPolicy) error {
	if p.Validate() != nil {
		return ErrFlowInvalid
	}
	db, err := e.flowDB()
	if err != nil {
		return err
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, old, err := readFlow(tx, id, true)
		if err != nil {
			return err
		}
		if revision == 0 || row.Revision != revision || revision == ^uint64(0) || !reflect.DeepEqual(old.Stages, p.Stages) {
			return ErrFlowConflict
		}
		b, _ := json.Marshal(p)
		return tx.Model(&models.TaskFlow{}).Where("id = ? AND revision = ?", id, revision).Updates(map[string]any{"revision": revision + 1, "policy": b}).Error
	})
	if err == nil {
		e.wakeDrainNow()
	}
	return err
}

// SubmitFlowTask atomically registers a task and its lineage before enqueueing.
// Children must be submitted synchronously by a processing parent. A URL already
// owned by a different run/parent is an explicit conflict, never silently reused.
// Repeated roots/children under the same parent are idempotent.
func (e *Engine) SubmitFlowTask(ctx context.Context, id string, parent uint, task *Task) error {
	if task == nil || task.Repeatable || task.Delay != 0 || !task.NotBefore.IsZero() {
		return ErrFlowInvalid
	}
	if err := e.validateTask(task); err != nil {
		return err
	}
	db, err := e.flowDB()
	if err != nil {
		return err
	}
	var saved models.CrawlerTask
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		flow, p, err := readFlow(tx, id, true)
		if err != nil {
			return err
		}
		lane := p.Stages[task.Stage]
		if flow.Site != task.Site || lane == "" {
			return ErrFlowInvalid
		}
		// Check both persistent identities, including duplicates after a lost commit acknowledgement.
		q := tx.Where("site = ? AND stage = ? AND url_hash = ?", task.Site, task.Stage, models.UrlHash(task.URL))
		if task.IdempotencyKey != "" {
			q = tx.Where("(site = ? AND stage = ? AND url_hash = ?) OR idempotency_hash = ?", task.Site, task.Stage, models.UrlHash(task.URL), models.IdempotencyKeyHash(task.IdempotencyKey))
		}
		if err = q.Take(&saved).Error; err == nil {
			if saved.FlowID != id || saved.PID != parent || saved.Site != task.Site || saved.Stage != task.Stage || saved.URL != task.URL {
				return ErrFlowConflict
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var par models.CrawlerTask
		if parent != 0 {
			if err = tx.Where("id = ? AND flow_id = ?", parent, id).Take(&par).Error; err != nil {
				return err
			}
			if par.Status != models.TaskStatusProcessing || strings.Count(par.FlowPath, "/") >= 32 {
				return ErrFlowConflict
			}
		}
		var count int64
		if err = tx.Model(&models.CrawlerTask{}).Where("flow_id = ?", id).Count(&count).Error; err != nil {
			return err
		}
		if count >= flowTaskLimit {
			return ErrFlowInvalid
		}
		saved = task.toModel()
		saved.PID = parent
		saved.FlowID = id
		saved.FlowLane = lane
		saved.FlowPath = ""
		saved.FlowBranch = 0
		if err = tx.Create(&saved).Error; err != nil {
			return err
		}
		saved.FlowPath = flowPath(par.FlowPath, saved.ID)
		saved.FlowBranch = saved.ID
		if par.FlowLane == lane {
			saved.FlowBranch = par.FlowBranch
		}
		return tx.Model(&models.CrawlerTask{}).Where("id = ?", saved.ID).Updates(map[string]any{"flow_path": saved.FlowPath, "flow_branch": saved.FlowBranch}).Error
	})
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
	db, err := e.flowDB()
	if err != nil {
		e.spillTask(task)
		return false, nil
	} // disabled: fail closed without occupying a worker
	claimed, waiting := false, false
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, p, err := readFlow(tx, task.FlowID, true)
		if err != nil {
			return err
		}
		var row models.CrawlerTask
		if err = tx.Where("id = ? AND flow_id = ?", task.ID, task.FlowID).Take(&row).Error; err != nil {
			return err
		}
		if row.Status != models.TaskStatusPending {
			return nil
		}
		var active []models.CrawlerTask
		if err = tx.Select("id", "p_id", "stage", "status", "flow_lane", "flow_branch", "flow_path").Where("flow_id = ? AND status IN ?", task.FlowID, []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}).Limit(flowTaskLimit + 1).Find(&active).Error; err != nil {
			return err
		}
		if len(active) > flowTaskLimit {
			return ErrFlowInvalid
		}
		allowed, _ := flowEligible(p, active, row)
		if !allowed {
			waiting = true
			return nil
		}
		result := claimScope(tx, uint(task.ID)).Updates(map[string]any{"status": models.TaskStatusProcessing, "urgent": false})
		claimed = result.RowsAffected == 1
		return result.Error
	})
	if err == nil && waiting {
		e.spillTask(task)
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
	var out TaskFlowView
	db, err := e.flowDB()
	if err != nil {
		return out, err
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, p, err := readFlow(tx, id, true)
		if err != nil {
			return err
		}
		out = TaskFlowView{ID: row.ID, Site: row.Site, Revision: row.Revision, Policy: p, Tasks: []FlowTaskView{}}
		var active []models.CrawlerTask
		if err = tx.Select("id", "p_id", "stage", "status", "flow_lane", "flow_branch", "flow_path").Where("flow_id = ? AND status IN ?", id, []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}).Order("id").Limit(flowTaskLimit + 1).Find(&active).Error; err != nil {
			return err
		}
		if len(active) > flowTaskLimit {
			return ErrFlowInvalid
		}
		out.ActiveCount = len(active)
		for i, r := range active {
			if i >= 200 {
				break
			}
			_, reason := flowEligible(p, active, r)
			if r.Status == models.TaskStatusProcessing {
				reason = "running; policy changes apply to next claim"
			}
			out.Tasks = append(out.Tasks, FlowTaskView{r.ID, r.Stage, r.FlowLane, r.FlowBranch, r.Status, reason})
		}
		return nil
	})
	return out, err
}
