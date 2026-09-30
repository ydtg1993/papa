package crawler

import (
	"time"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// RepollRepeatableTasks 重新投递「已完成」的 repeatable 任务（success/failed），返回实际投递数量。
// 只重投 success/failed，不碰还在 pending/processing 的——后者由 recover_queue 兜底，避免双入队。
// 分页流式 + 并发处理（复用 processInBatches），并发数与批大小取 repeat_queue 配置。
func (e *Engine) RepollRepeatableTasks() (int, error) {
	e.repeatQueueMu.Lock()
	defer e.repeatQueueMu.Unlock()

	cfg := e.repeatQueueConfig()
	query := func() *gorm.DB {
		return e.db.Where("repeatable = ? AND status IN ?",
			models.RepeatableYes,
			[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed})
	}
	return e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueRepeatTask)
}

// requeueRepeatTask 将单条 repeatable 任务重置为 pending 并重新投递。
func (e *Engine) requeueRepeatTask(t *models.CrawlerTask) bool {
	if e.stages[t.Stage] == nil {
		e.loggerSet.Engine.Warnf("repeat queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	task := &Task{
		ID:             int(t.ID),
		PID:            int(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Repeatable:     true,
		IdempotencyKey: t.IdempotencyKey,
	}
	task.UpdateStatus(e.db, models.TaskStatusPending, nil)
	if err := e.SubmitTask(task); err != nil {
		e.loggerSet.Engine.Errorf("repeat queue: submit task %d: %s", t.ID, err.Error())
		return false
	}
	return true
}

// startRepeatQueue 启动后台定时重新投递 repeatable 任务（enabled/interval 运行期可热更）。
func (e *Engine) startRepeatQueue() {
	e.runDynamicTicker(func() time.Duration {
		cfg := e.repeatQueueConfig()
		if !cfg.Enabled || cfg.Interval <= 0 {
			return 0
		}
		return cfg.Interval
	}, func() {
		if n, err := e.RepollRepeatableTasks(); err != nil {
			e.loggerSet.Engine.Errorf("repeat queue: auto repoll: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("repeat queue: auto repolled %d tasks", n)
		}
	})
}
