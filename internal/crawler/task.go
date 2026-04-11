package crawler

import (
	"context"
	"github.com/ydtg1993/papa/internal/models"
	"gorm.io/gorm"
)

type Task struct {
	ID         int `json:"id"`  // 数据库记录 ID
	PID        int `json:"pid"` // 父级任务ID
	URL        string
	Retry      int
	Stage      string //阶段标识，如 "catalog", "detail", "video"
	Repeatable bool
}

func (t *Task) GetUrl() string {
	return t.URL
}

func (t *Task) GetRetry() int {
	return t.Retry
}

func (t *Task) IncRetry(db *gorm.DB) {
	t.Retry++
	db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("retry", gorm.Expr("retry + ?", 1))
}

func (t *Task) Insert(db *gorm.DB) bool {
	repeat := models.RepeatableNo
	if t.Repeatable {
		repeat = models.RepeatableYes
	}
	crawlerTask := models.CrawlerTask{
		PID:        uint(t.PID),
		URL:        t.URL,
		Stage:      t.Stage,
		Repeatable: repeat,
		Status:     models.TaskStatusPending,
	}
	if err := db.Create(&crawlerTask).Error; err != nil {
		return false
	}
	t.ID = int(crawlerTask.ID)
	return true
}

func (t *Task) UpdateStatus(db *gorm.DB, status models.TaskStatus, err error) bool {
	if t.ID == 0 {
		return false
	}
	var record models.CrawlerTask
	db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).First(&record)
	if record.ID <= 0 {
		return false
	}
	if status == models.TaskStatusFailed {
		var errMsg string
		if err != nil {
			errMsg = err.Error()
		}
		record.Status = status
		record.Error += errMsg + "\r"
		db.Save(&record)
		return true
	}
	record.Status = status
	db.Save(&record)
	return true
}

func (t *Task) Unique() string {
	return t.Stage + "|" + t.URL
}

// Fetcher 爬虫操作业务逻辑接口
type Fetcher interface {
	GetStage() string
	FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
