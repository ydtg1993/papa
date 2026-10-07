package engine

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
	Urgent         bool              // 加急：投到所属阶段的快车道，插到常规队列前面

	// Trace 本次尝试的步骤记录器，由 worker 在调用 FetchHandler 前挂上。
	// 追踪未开启时为 nil，Step/Fail 是安全的 no-op。handler 无需判空。
	Trace *Trace
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
		Urgent:         t.Urgent,
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

// UpdateStatus 写入任务状态；status 为 failed 时把错误追加到 error 列。
//
// **只更新 status / error 两列，不整行 Save**：worker 标状态和业务回写结果是两条并发的路
// （`SaveResult` / `SaveContent` 也是按列写的），整行写会把这里读到的旧快照盖回去 ——
// 业务刚写进 content 的内容就这么没了。追加错误同理，走 SQL 的 CONCAT，不把行读出来拼。
//
// 返回值是「这条语句执行成功了」，**不是**「行存在」—— 原来返回 false 表示行不见了，
// 但没有任何调用方用它判存在性，别依赖它。也正因为不需要判存在性，这里少了一次 SELECT。
func (t *Task) UpdateStatus(db *gorm.DB, status models.TaskStatus, err error) bool {
	if t.ID == 0 {
		return false
	}
	updates := map[string]any{"status": status}
	if status == models.TaskStatusFailed {
		var errMsg string
		if err != nil {
			errMsg = err.Error()
		}
		updates["error"] = gorm.Expr("CONCAT(COALESCE(error, ''), ?)", errMsg+"\n")
	}
	return db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).Updates(updates).Error == nil
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
