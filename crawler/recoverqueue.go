package crawler

import (
	"time"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// ProcessRecoverQueue 查询「卡死」的 pending/processing 任务（updated_at 早于 now-timeout）并重新投递，
// 返回实际恢复的数量。供启动时、定时轮询、OA 后台手动触发复用。
func (e *Engine) ProcessRecoverQueue() (int, error) {
	cfg := e.recoverQueueConfig()
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}
	cutoff := time.Now().Add(-timeout)

	query := func() *gorm.DB {
		return e.db.Where("(status = ? OR status = ?) AND updated_at < ?",
			models.TaskStatusPending, models.TaskStatusProcessing, cutoff)
	}
	return e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueRecoverTask)
}

// requeueRecoverTask 将单条卡死任务重置为 pending 并重新投递；提交失败则标 failed。
func (e *Engine) requeueRecoverTask(t *models.CrawlerTask) bool {
	if e.stages[t.Stage] == nil {
		e.loggerSet.Engine.Warnf("recover queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	task := &Task{
		ID:             int(t.ID),
		PID:            int(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Retry:          t.Retry,
		Repeatable:     false,
		IdempotencyKey: t.IdempotencyKey,
	}
	// 剔除去重表暂存，再按「已入库重提交」路径重新入队
	e.DelActiveTask(task)
	task.UpdateStatus(e.db, models.TaskStatusPending, nil)
	if err := e.SubmitTask(task); err != nil {
		e.loggerSet.Engine.Errorf("recover queue: submit task %d: %s", t.ID, err.Error())
		e.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).Updates(map[string]any{
			"status": models.TaskStatusFailed,
			"retry":  gorm.Expr("retry + 1"),
			"error":  gorm.Expr("CONCAT(COALESCE(error, ''), ?)", "RecoverQueue 恢复任务提交失败\n"),
		})
		return false
	}
	e.recoveredCount.Add(1)
	return true
}

// startRecoverQueue 若启用：启动时立即恢复一次；配置了 interval 再启动后台定时轮询。
func (e *Engine) startRecoverQueue() {
	// 启动时立即恢复一次（仅启动时；异步避免阻塞启动）
	if e.recoverQueueConfig().Enabled {
		go func() {
			if n, err := e.ProcessRecoverQueue(); err != nil {
				e.loggerSet.Engine.Errorf("recover queue: startup recover: %s", err.Error())
			} else if n > 0 {
				e.loggerSet.Engine.Infof("recover queue: startup recovered %d tasks", n)
			}
		}()
	}

	// 定时轮询（enabled/interval 运行期可热更）
	e.runDynamicTicker(func() time.Duration {
		cfg := e.recoverQueueConfig()
		if !cfg.Enabled || cfg.Interval <= 0 {
			return 0
		}
		return cfg.Interval
	}, func() {
		if n, err := e.ProcessRecoverQueue(); err != nil {
			e.loggerSet.Engine.Errorf("recover queue: auto recover: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("recover queue: auto recovered %d tasks", n)
		}
	})
}
