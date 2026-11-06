package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/models"
)

// Explicitly simulated state for browser verification; never contacts a DB/site.
type demoSchedules struct {
	mu      sync.Mutex
	paused  bool
	retried bool
	version int
}

func (d *demoSchedules) snapshot() engine.RuleScheduleDetail {
	next := time.Now().Add(10 * time.Minute)
	c := engine.RuleScheduleTask{ID: 101, Site: "shop", Stage: "schedule", Status: models.TaskStatusSuccess, Repeatable: models.RepeatableYes, RepeatInterval: 600, NextRepeatAt: &next}
	if d.paused {
		c.Repeatable = models.RepeatableNo
	}
	m := engine.RuleScheduleTask{ID: 102, Site: "shop", Stage: "detail", Status: models.TaskStatusFailed, Reprocess: d.version}
	b := engine.RuleScheduleStatus{TaskID: 101, Revision: 1, Sequence: 3, BatchID: strings.Repeat("a", 64), RootID: 102, ReleaseID: "demo-release-v3", ReleaseGeneration: 3, Total: 1, Failed: 1}
	if d.retried {
		m.Status = models.TaskStatusSuccess
		b.Failed = 0
		b.Success = 1
		b.Complete = true
	}
	return engine.RuleScheduleDetail{Control: c, Batch: b, Rules: map[string]engine.RuleRef{"detail": {ID: "demo-detail", Version: "v3"}}, Members: []engine.RuleScheduleTask{m}}
}
func (d *demoSchedules) list(_ context.Context, site string, after uint) (engine.RuleScheduleList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows := []engine.RuleScheduleTask{}
	if (site == "" || site == "shop") && after < 101 {
		rows = append(rows, d.snapshot().Control)
	}
	return engine.RuleScheduleList{Items: rows}, nil
}
func (d *demoSchedules) detail(_ context.Context, id, after uint, batch string, failed bool) (engine.RuleScheduleDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id != 101 {
		return engine.RuleScheduleDetail{}, engine.ErrTaskNotFound
	}
	x := d.snapshot()
	if batch != "" && batch != x.Batch.BatchID {
		return x, engine.ErrRuleScheduleConflict
	}
	if after >= 102 || failed && d.retried {
		x.Members = []engine.RuleScheduleTask{}
	}
	return x, nil
}
func (d *demoSchedules) table() oao.Table {
	t := fakeTaskTable()
	for _, key := range []string{"retry", "repeat_on", "repeat_off", "trace"} {
		action := key
		t.Actions = append(t.Actions, oao.Action{Key: action, Label: map[string]string{"retry": "模拟重试", "repeat_on": "模拟恢复轮询", "repeat_off": "模拟停轮询", "trace": "追踪"}[action], Handler: func(_ context.Context, req oao.ActionRequest) error {
			d.mu.Lock()
			defer d.mu.Unlock()
			if action == "trace" {
				return nil
			}
			if req.ID != "101" && req.ID != "102" {
				return oao.Fail(http.StatusConflict, "此演示动作只支持周期批次页的模拟任务")
			}
			if action == "retry" && req.ID == "102" {
				if req.RowString("reprocess") != strconv.Itoa(d.version) || d.retried {
					return oao.Fail(http.StatusConflict, "模拟任务已变化")
				}
				d.retried = true
				d.version++
				return nil
			}
			if req.ID != "101" {
				return oao.Fail(http.StatusConflict, "批次成员不能单独轮询")
			}
			if action == "repeat_off" {
				d.paused = true
			}
			if action == "repeat_on" {
				d.paused = false
			}
			return nil
		}})
	}
	return t
}
