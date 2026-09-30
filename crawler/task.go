package crawler

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

type Task struct {
	ID             int    `json:"id"`  // 数据库记录 ID
	PID            int    `json:"pid"` // 父级任务ID
	URL            string // 要打开的 URL
	Retry          int
	Stage          string // 阶段标识，如 "catalog", "detail", "video"
	Repeatable     bool
	Meta           map[string]string // 业务键（如 series_id/episode_id），与 URL 解耦
	IdempotencyKey string            // 自定义幂等键，为空时回退 Stage|URL
	NotBefore      time.Time         // 延迟投递：最早可执行时间
	Delay          time.Duration     // 延迟投递：相对当前时间的延迟
}

// deliverAt 返回任务的延迟投递时间；无延迟时返回零值 time.Time。
func (t *Task) deliverAt() time.Time {
	if !t.NotBefore.IsZero() {
		return t.NotBefore
	}
	if t.Delay > 0 {
		return time.Now().Add(t.Delay)
	}
	return time.Time{}
}

func (t *Task) IncRetry(db *gorm.DB) {
	t.Retry++
	db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).
		Update("retry", gorm.Expr("retry + ?", 1))
}

// toModel 将任务转换为数据库记录（不含 ID，由数据库生成）。
func (t *Task) toModel() models.CrawlerTask {
	repeat := models.RepeatableNo
	if t.Repeatable {
		repeat = models.RepeatableYes
	}
	return models.CrawlerTask{
		PID:            uint(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		IdempotencyKey: t.IdempotencyKey,
		Repeatable:     repeat,
		Status:         models.TaskStatusPending,
	}
}

func (t *Task) Insert(db *gorm.DB) error {
	crawlerTask := t.toModel()
	if err := db.Create(&crawlerTask).Error; err != nil {
		return err
	}
	t.ID = int(crawlerTask.ID)
	return nil
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
		record.Error += errMsg + "\n"
		db.Save(&record)
		return true
	}
	record.Status = status
	db.Save(&record)
	return true
}

func (t *Task) Unique() string {
	if t.IdempotencyKey != "" {
		return t.IdempotencyKey
	}
	return t.Stage + "|" + t.URL
}

// Fetcher 爬虫操作业务逻辑接口
type Fetcher interface {
	GetStage() string
	FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}
