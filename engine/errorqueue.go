package engine

import (
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// errorQueueQuery 失败任务的查询条件：status=failed，配置了上限时再处理代数未超 max_retry。
//
// **必须带上 Model**：这个构造器有两个消费方 —— `processInBatches` 的 `Find(&tasks)`
// （切片元素类型能推出表名，带不带都行）和 `sampleQueueBacklogOne` 的 `Count(&n)`
// （**gorm 的 Count 推不出表名**，没有 Model 就直接报 "Table not set"）。
// 漏掉 Model 的后果不是报错，而是积压数恒为 0 + 每轮采样往日志写一条 WARN。
func (e *Engine) errorQueueQuery() func() *gorm.DB {
	cfg := e.errorQueueConfig()
	return func() *gorm.DB {
		q := e.db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusFailed)
		if cfg.MaxRetry > 0 {
			q = q.Where("reprocess < ?", cfg.MaxRetry)
		}
		return q
	}
}

// ProcessErrorQueue 查询失败任务并重新投递到各自阶段，返回实际重新投递的数量。
func (e *Engine) ProcessErrorQueue() (int, error) {
	e.errorQueueMu.Lock()
	defer e.errorQueueMu.Unlock()

	cfg := e.errorQueueConfig()
	query := e.errorQueueQuery()
	e.beginQueueRun(QueueError)
	n, err := e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueFailedTask)
	e.endQueueRun(QueueError, n, err)
	// 本轮到点即采样一次积压，监控页无需等下一轮采样周期
	e.sampleQueueBacklogOne(QueueError, query)
	return n, err
}

// requeueFailedTask 将单条失败任务重置为 pending 并重新投递到其阶段工作池。
func (e *Engine) requeueFailedTask(t *models.CrawlerTask) bool {
	info := e.stages[t.Stage]
	if info == nil {
		e.loggerSet.Engine.Warnf("error queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	if err := e.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).Updates(map[string]any{
		"status":    models.TaskStatusPending,
		"retry":     0,
		"reprocess": gorm.Expr("reprocess + 1"),
	}).Error; err != nil {
		e.loggerSet.Engine.Errorf("error queue: reset task %d: %s", t.ID, err.Error())
		return false
	}

	task := e.taskFromRecord(t)
	task.Retry = 0 // 上面刚把行里的 retry 归零，内存副本跟上，别让 handler 读到已经作废的重试次数
	e.dedupCache.Add(task.Unique())
	if err := e.submitTo(info, task); err != nil {
		e.dedupCache.Delete(task.Unique())
		e.loggerSet.Engine.Errorf("error queue: submit task %d: %s", t.ID, err.Error())
		// 上面刚把这行重置成 pending，失败不管的话它就再也没人捞了（本队列只捞 failed）
		e.markRequeueFailed(QueueError, t.ID, err)
		return false
	}
	e.errorRetriedCount.Add(1)
	return true
}

// startErrorQueue 启动后台定时自动处理失败任务（enabled/interval 运行期可热更）。
func (e *Engine) startErrorQueue() {
	e.runDynamicTicker(func() time.Duration {
		cfg := e.errorQueueConfig()
		if !cfg.Enabled || cfg.Interval <= 0 {
			return 0
		}
		return cfg.Interval
	}, func() {
		if n, err := e.ProcessErrorQueue(); err != nil {
			e.loggerSet.Engine.Errorf("error queue: auto process: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("error queue: auto processed %d failed tasks", n)
		}
	})
}
