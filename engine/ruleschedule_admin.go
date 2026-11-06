package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

const ruleScheduleAdminPageSize = 20
const ruleScheduleMemberPageSize = 50

// RuleScheduleTask is a credential-free task projection for operations pages.
type RuleScheduleTask struct {
	ID             uint                    `json:"id,string"`
	Site           string                  `json:"site"`
	Stage          string                  `json:"stage"`
	Status         models.TaskStatus       `json:"status"`
	Reprocess      int                     `json:"reprocess"`
	Repeatable     models.RepeatableStatus `json:"repeatable"`
	RepeatInterval int                     `json:"repeat_interval"`
	NextRepeatAt   *time.Time              `json:"next_repeat_at"`
	UpdatedAt      time.Time               `json:"updated_at"`
}

type RuleScheduleList struct {
	Items []RuleScheduleTask `json:"items"`
	Next  uint               `json:"next,string"` // last ID; zero means end
}

type RuleScheduleDetail struct {
	Control RuleScheduleTask   `json:"control"`
	Batch   RuleScheduleStatus `json:"batch"`
	Rules   map[string]RuleRef `json:"rules"`
	Members []RuleScheduleTask `json:"members"`
	Next    uint               `json:"next,string"`
}

// MarshalJSON preserves Go's status API while keeping identifiers and counters
// exact in browser clients (JavaScript Number cannot represent all uint64s).
func (d RuleScheduleDetail) MarshalJSON() ([]byte, error) {
	type detail RuleScheduleDetail
	type batch struct {
		RuleScheduleStatus
		TaskID            uint   `json:"TaskID,string"`
		RootID            uint   `json:"RootID,string"`
		Revision          uint64 `json:"Revision,string"`
		Sequence          uint64 `json:"Sequence,string"`
		ReleaseGeneration uint64 `json:"ReleaseGeneration,string"`
	}
	b := d.Batch
	return json.Marshal(struct {
		detail
		Batch batch `json:"batch"`
	}{detail(d), batch{b, b.TaskID, b.RootID, b.Revision, b.Sequence, b.ReleaseGeneration}})
}

func scheduleAdminScope(db *gorm.DB, site string, after uint) *gorm.DB {
	q := db.Model(&models.CrawlerTask{}).Where("id > ? AND JSON_CONTAINS_PATH(meta, 'one', '$._rule_schedule') = 1", after)
	if site != "" {
		q = q.Where("site = ?", site)
	}
	return q.Order("id ASC")
}

const scheduleTaskColumns = "id, site, stage, status, reprocess, repeatable, repeat_interval, next_repeat_at, updated_at"

// ListRuleSchedules reads controllers, including stopped/unregistered ones.
// No per-controller aggregates are executed in this bounded list query.
func (e *Engine) ListRuleSchedules(ctx context.Context, site string, after uint) (RuleScheduleList, error) {
	out := RuleScheduleList{Items: []RuleScheduleTask{}}
	if ctx == nil || len(site) > 128 {
		return out, ErrRuleScheduleInvalid
	}
	db, err := scheduleDB(e)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = scheduleAdminScope(db.WithContext(ctx), site, after).Select(scheduleTaskColumns).Limit(ruleScheduleAdminPageSize + 1).Scan(&out.Items).Error
	if len(out.Items) > ruleScheduleAdminPageSize {
		out.Items = out.Items[:ruleScheduleAdminPageSize]
		out.Next = out.Items[len(out.Items)-1].ID
	}
	return out, err
}

func scheduleTaskProjection(t models.CrawlerTask) RuleScheduleTask {
	return RuleScheduleTask{ID: t.ID, Site: t.Site, Stage: t.Stage, Status: t.Status,
		Reprocess: t.Reprocess, Repeatable: t.Repeatable, RepeatInterval: t.RepeatInterval,
		NextRepeatAt: t.NextRepeatAt, UpdatedAt: t.UpdatedAt}
}

// RuleScheduleDetail reads control, publication and members in one repeatable
// read snapshot. The caller may pin batchID for member pagination; a changed
// batch returns conflict rather than mixing members of different executions.
func (e *Engine) RuleScheduleDetail(ctx context.Context, id, after uint, batchID string, failedOnly bool) (RuleScheduleDetail, error) {
	out := RuleScheduleDetail{Rules: map[string]RuleRef{}, Members: []RuleScheduleTask{}}
	if ctx == nil || id == 0 || (batchID != "" && !validRuleBatch(batchID)) || (after != 0 && batchID == "") {
		return out, ErrRuleScheduleInvalid
	}
	db, err := scheduleDB(e)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row models.CrawlerTask
		if err := tx.First(&row, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTaskNotFound
			}
			return err
		}
		if RuleTaskRole(row.Meta) != "schedule" {
			return ErrRuleScheduleInvalid
		}
		stage := e.stages[stageKeyOfRecord(&row)]
		if stage == nil {
			return ErrStageNotRegistered
		}
		s, ok := stage.fetcher.(*RuleSchedule)
		if !ok || s == nil {
			return ErrStageNotRegistered
		}
		c, state, err := s.readSchedule(ctx, tx, id)
		if err != nil {
			return err
		}
		out.Control = scheduleTaskProjection(c.CrawlerTask)
		out.Batch, err = s.batchStatus(ctx, tx, c, state)
		if err != nil {
			return err
		}
		if batchID != "" && batchID != out.Batch.BatchID {
			return ErrRuleScheduleConflict
		}
		if state == nil {
			return nil
		}
		if err = ruleDecode(state.payload.Root.Meta[ruleBindingsMeta], &out.Rules); err != nil {
			return ErrRuleScheduleCorrupt
		}
		lower := out.Batch.RootID
		if lower == 0 {
			lower = c.ID
		}
		q := scheduleBatchScope(tx, c.Site, out.Batch.BatchID, lower).Where("id > ?", after)
		if failedOnly {
			q = q.Where("status = ?", models.TaskStatusFailed)
		}
		if err = q.Select(scheduleTaskColumns).Order("id ASC").Limit(ruleScheduleMemberPageSize + 1).Scan(&out.Members).Error; err != nil {
			return err
		}
		if len(out.Members) > ruleScheduleMemberPageSize {
			out.Members = out.Members[:ruleScheduleMemberPageSize]
			out.Next = out.Members[len(out.Members)-1].ID
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return out, err
}
