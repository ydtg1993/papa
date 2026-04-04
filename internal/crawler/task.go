package crawler

import (
	"context"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/models"
	"gorm.io/gorm"
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

func (t *Task) IncRetry(db *gorm.DB, logger *logrus.Logger) {
	t.Retry++
	upErr := db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("retry", gorm.Expr("retry + ?", 1))
	if upErr != nil {
		logger.Errorf("increment crawler task retry failed: %v", upErr)
	}
}

func (t *Task) Insert(db *gorm.DB, logger *logrus.Logger) bool {
	crawlerTask := models.CrawlerTask{
		URL:    t.URL,
		Stage:  t.Stage,
		Retry:  0,
		Status: models.TaskStatusPending,
	}
	if err := db.Create(&crawlerTask).Error; err != nil {
		logger.Errorf("insert crawler task to db failed: %v", err)
		return false
	}
	t.ID = int64(crawlerTask.ID)
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
			logger.Errorf("crawler task update status error: %v", result.Error)
			return false
		}
		return true
	}
	upErr := db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("status", status)
	if upErr != nil {
		logger.Errorf("crawler task update status error: %v", upErr)
		return false
	}
	return true
}

func (t *Task) Unique() string {
	return t.Stage + "|" + t.URL
}

type Handler interface {
	GetStage() string
	FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
