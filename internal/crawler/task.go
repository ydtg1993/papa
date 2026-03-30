package crawler

import (
	"context"
	"github.com/ydtg1993/papa/internal/models"
	"github.com/ydtg1993/papa/pkg/database"
)

type Task struct {
	ID    int64 `json:"id"` // 数据库记录 ID
	URL   string
	Retry int
	Stage string //阶段标识，如 "catalog", "detail"
}

func (t *Task) GetUrl() string {
	return t.URL
}

func (t *Task) GetRetry() int {
	return t.Retry
}

func (t *Task) IncRetry() {
	t.Retry++
}

func (t *Task) UpdateStatus(status models.TaskStatus, err error) {
	if t.ID == 0 {
		return
	}
	if status == models.TaskStatusFailed {
		var errMsg string
		if err != nil {
			errMsg = err.Error()
		}
		database.DB.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
			Updates(map[string]interface{}{
				"status": status,
				"error":  errMsg,
			})
		return
	}
	database.DB.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("status", status)
}

type Handler interface {
	GetStage() string
	FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
