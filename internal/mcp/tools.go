package mcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/models"
)

// submit_task
type submitTaskInput struct {
	Stage      string `json:"stage" jsonschema:"阶段标识,如 first/second,必须与配置一致"`
	URL        string `json:"url" jsonschema:"目标 URL"`
	Repeatable bool   `json:"repeatable,omitempty" jsonschema:"是否轮询任务"`
	PID        int    `json:"pid,omitempty" jsonschema:"父任务 ID,默认 0"`
}

type submitTaskOutput struct {
	TaskID  int    `json:"task_id" jsonschema:"入库后的任务 ID"`
	Message string `json:"message" jsonschema:"结果说明"`
}

func submitTaskTool(e *crawler.Engine) sdkmcp.ToolHandlerFor[submitTaskInput, submitTaskOutput] {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, in submitTaskInput) (*sdkmcp.CallToolResult, submitTaskOutput, error) {
		task := &crawler.Task{PID: in.PID, URL: in.URL, Stage: in.Stage, Repeatable: in.Repeatable}
		if err := e.SubmitTask(task); err != nil {
			return nil, submitTaskOutput{}, err
		}
		return nil, submitTaskOutput{TaskID: task.ID, Message: "submitted"}, nil
	}
}

// list_stages
type stageInfo struct {
	Name        string `json:"name"`
	WorkerCount int    `json:"worker_count"`
	QueueSize   int    `json:"queue_size"`
	Delay       string `json:"delay"`
	MaxAttempts int    `json:"max_attempts"`
	Backoff     string `json:"backoff"`
}

type listStagesOutput struct {
	Stages []stageInfo `json:"stages"`
}

func listStagesTool(e *crawler.Engine) sdkmcp.ToolHandlerFor[struct{}, listStagesOutput] {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, listStagesOutput, error) {
		cfgStages := e.GetConfig().Crawler.Stages
		names := make([]string, 0, len(cfgStages))
		for name := range cfgStages {
			names = append(names, name)
		}
		sort.Strings(names)

		out := listStagesOutput{Stages: make([]stageInfo, 0, len(names))}
		for _, name := range names {
			c := cfgStages[name]
			out.Stages = append(out.Stages, stageInfo{
				Name:        name,
				WorkerCount: c.WorkerCount,
				QueueSize:   c.QueueSize,
				Delay:       c.Delay.String(),
				MaxAttempts: c.Retry.MaxAttempts,
				Backoff:     c.Retry.Backoff.String(),
			})
		}
		return nil, out, nil
	}
}

// get_stats
type globalStatsOut struct {
	TotalTasks  int64 `json:"total_tasks"`
	TotalFailed int64 `json:"total_failed"`
	AvgMs       int64 `json:"avg_ms"`
	MaxMs       int64 `json:"max_ms"`
	MinMs       int64 `json:"min_ms"`
}

type workerStatsOut struct {
	WorkerID    int   `json:"worker_id"`
	TotalTasks  int64 `json:"total_tasks"`
	FailedTasks int64 `json:"failed_tasks"`
	TotalMs     int64 `json:"total_ms"`
	MaxMs       int64 `json:"max_ms"`
	MinMs       int64 `json:"min_ms"`
}

type stageStatsOut struct {
	Stage   string           `json:"stage"`
	Global  globalStatsOut   `json:"global"`
	Workers []workerStatsOut `json:"workers"`
}

type getStatsOutput struct {
	Stages []stageStatsOut `json:"stages"`
}

func getStatsTool(e *crawler.Engine) sdkmcp.ToolHandlerFor[struct{}, getStatsOutput] {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, getStatsOutput, error) {
		monitors := e.GetStatsQueue()
		names := make([]string, 0, len(monitors))
		for name := range monitors {
			names = append(names, name)
		}
		sort.Strings(names)

		out := getStatsOutput{Stages: make([]stageStatsOut, 0, len(names))}
		for _, name := range names {
			mon := monitors[name]
			g := mon.GetGlobalStats()
			ws := mon.GetAllWorkerStats()
			workers := make([]workerStatsOut, 0, len(ws))
			for _, w := range ws {
				workers = append(workers, workerStatsOut{
					WorkerID:    w.WorkerID,
					TotalTasks:  w.TotalTasks,
					FailedTasks: w.FailedTasks,
					TotalMs:     w.TotalTime.Milliseconds(),
					MaxMs:       w.MaxTime.Milliseconds(),
					MinMs:       w.MinTime.Milliseconds(),
				})
			}
			out.Stages = append(out.Stages, stageStatsOut{
				Stage: name,
				Global: globalStatsOut{
					TotalTasks:  g.TotalTasks,
					TotalFailed: g.TotalFailed,
					AvgMs:       g.AvgTime.Milliseconds(),
					MaxMs:       g.MaxTime.Milliseconds(),
					MinMs:       g.MinTime.Milliseconds(),
				},
				Workers: workers,
			})
		}
		return nil, out, nil
	}
}

// list_tasks
type listTasksInput struct {
	Stage  string `json:"stage,omitempty" jsonschema:"按阶段过滤,空为全部"`
	Status string `json:"status,omitempty" jsonschema:"pending/processing/success/failed,空为全部"`
	Limit  int    `json:"limit,omitempty" jsonschema:"最大返回条数,默认 50,最大 500"`
}

type taskSummary struct {
	ID         uint   `json:"id"`
	PID        uint   `json:"pid"`
	Stage      string `json:"stage"`
	URL        string `json:"url"`
	Status     string `json:"status"`
	Retry      int    `json:"retry"`
	Repeat     int    `json:"repeat"`
	Repeatable bool   `json:"repeatable"`
	Error      string `json:"error"`
}

type listTasksOutput struct {
	Tasks []taskSummary `json:"tasks"`
}

func listTasksTool(e *crawler.Engine) sdkmcp.ToolHandlerFor[listTasksInput, listTasksOutput] {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, in listTasksInput) (*sdkmcp.CallToolResult, listTasksOutput, error) {
		limit := in.Limit
		if limit <= 0 || limit > 500 {
			limit = 50
		}

		q := e.GetDB().Model(&models.CrawlerTask{})
		if in.Stage != "" {
			q = q.Where("stage = ?", in.Stage)
		}
		if in.Status != "" {
			st, ok := parseStatus(in.Status)
			if !ok {
				return nil, listTasksOutput{}, fmt.Errorf("invalid status: %s", in.Status)
			}
			q = q.Where("status = ?", st)
		}

		var tasks []models.CrawlerTask
		if err := q.Order("id desc").Limit(limit).Find(&tasks).Error; err != nil {
			return nil, listTasksOutput{}, err
		}

		out := listTasksOutput{Tasks: make([]taskSummary, 0, len(tasks))}
		for _, t := range tasks {
			out.Tasks = append(out.Tasks, taskSummary{
				ID:         t.ID,
				PID:        t.PID,
				Stage:      t.Stage,
				URL:        t.URL,
				Status:     statusString(t.Status),
				Retry:      t.Retry,
				Repeat:     t.Repeat,
				Repeatable: t.Repeatable == models.RepeatableYes,
				Error:      t.Error,
			})
		}
		return nil, out, nil
	}
}

// resubmit_task
type resubmitTaskInput struct {
	URL   string `json:"url" jsonschema:"已入库任务的 URL"`
	Stage string `json:"stage" jsonschema:"阶段标识"`
}

type resubmitTaskOutput struct {
	Message string `json:"message"`
}

func resubmitTaskTool(e *crawler.Engine) sdkmcp.ToolHandlerFor[resubmitTaskInput, resubmitTaskOutput] {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, in resubmitTaskInput) (*sdkmcp.CallToolResult, resubmitTaskOutput, error) {
		if err := e.ReSubmitTask(&crawler.Task{URL: in.URL, Stage: in.Stage}); err != nil {
			return nil, resubmitTaskOutput{}, err
		}
		return nil, resubmitTaskOutput{Message: "resubmitted"}, nil
	}
}

// recover_tasks
type recoverTasksOutput struct {
	Recovered int `json:"recovered" jsonschema:"成功恢复数量"`
	Failed    int `json:"failed" jsonschema:"恢复失败数量"`
}

func recoverTasksTool(e *crawler.Engine) sdkmcp.ToolHandlerFor[struct{}, recoverTasksOutput] {
	return func(ctx context.Context, req *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, recoverTasksOutput, error) {
		var tasks []models.CrawlerTask
		timeout := time.Now().Add(-6 * time.Hour)
		if err := e.GetDB().Where("(status = ? OR status = ?) AND updated_at < ?",
			models.TaskStatusPending, models.TaskStatusProcessing, timeout).Find(&tasks).Error; err != nil {
			return nil, recoverTasksOutput{}, err
		}

		out := recoverTasksOutput{}
		for _, t := range tasks {
			task := &crawler.Task{ID: int(t.ID), PID: int(t.PID), URL: t.URL, Stage: t.Stage, Retry: t.Retry}
			e.DelActiveTask(task)
			task.UpdateStatus(e.GetDB(), models.TaskStatusPending, nil)
			if err := e.SubmitTask(task); err != nil {
				out.Failed++
			} else {
				out.Recovered++
			}
		}
		return nil, out, nil
	}
}

func parseStatus(s string) (models.TaskStatus, bool) {
	switch s {
	case "pending":
		return models.TaskStatusPending, true
	case "processing":
		return models.TaskStatusProcessing, true
	case "success":
		return models.TaskStatusSuccess, true
	case "failed":
		return models.TaskStatusFailed, true
	}
	return 0, false
}

func statusString(s models.TaskStatus) string {
	switch s {
	case models.TaskStatusPending:
		return "pending"
	case models.TaskStatusProcessing:
		return "processing"
	case models.TaskStatusSuccess:
		return "success"
	case models.TaskStatusFailed:
		return "failed"
	}
	return "unknown"
}
