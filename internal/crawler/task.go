package crawler

import (
	"context"
	"github.com/sirupsen/logrus"
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

func (t *Task) IncRetry(db *gorm.DB, logger *logrus.Logger) {
	t.Retry++
	upErr := db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("retry", gorm.Expr("retry + ?", 1)).Error
	if upErr != nil {
		logger.Errorf("increment crawler task retry failed: %s", upErr.Error())
	}
}

func (t *Task) Insert(db *gorm.DB, logger *logrus.Logger) bool {
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
		logger.Errorf("insert crawler task to db failed: %s", err.Error())
		return false
	}
	t.ID = int(crawlerTask.ID)
	return true
}

func (t *Task) UpdateStatus(db *gorm.DB, logger *logrus.Logger, status models.TaskStatus, err error) bool {
	if t.ID == 0 {
		return false
	}
	if status == models.TaskStatusFailed {
		var errMsg string
		if err != nil {
			errMsg = err.Error()
		}
		result := db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
			Updates(map[string]interface{}{
				"status": status,
				"error":  errMsg,
			})
		if result.Error != nil {
			logger.Errorf("crawler task update status error: %s", result.Error.Error())
			return false
		}
		return true
	}
	upErr := db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("status", status).Error
	if upErr != nil {
		logger.Errorf("crawler task update status error: %s", upErr.Error())
		return false
	}
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
