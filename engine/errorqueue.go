package engine

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/events"
	"gorm.io/gorm"
)

// errorQueueState 错误队列（全局一条）的运行态：自己的锁（自动与手动不会互相插队）与累计计数。
type errorQueueState struct {
	mu      sync.Mutex
	retried atomic.Int64 // 累计重新投递数（同时就是监控里那一行的"处理量"）
}

// errorQueueQuery 失败任务的查询条件：status=failed，配了上限时再处理代数未超 max_retry。
//
// **必须带上 Model**：这个构造器有两个消费方 —— `processInBatches` 的 `Find(&tasks)`
// （切片元素类型能推出表名，带不带都行）和 `sampleQueueBacklogOne` 的 `Count(&n)`
// （**gorm 的 Count 推不出表名**，没有 Model 就直接报 "Table not set"）。
func (e *Engine) errorQueueQuery() func() *gorm.DB {
	cfg := e.cfg.ErrorQueue
	return func() *gorm.DB {
		q := e.db.Model(&models.CrawlerTask{}).
			Where("status = ?", models.TaskStatusFailed)
		if cfg.MaxRetry > 0 {
			q = q.Where("reprocess < ?", cfg.MaxRetry)
		}
		return q
	}
}

// ProcessErrorQueue 查询失败任务并重新投递到各自阶段，返回实际重新投递的数量。
func (e *Engine) ProcessErrorQueue() (int, error) {
	st := e.errorQueue
	st.mu.Lock()
	defer st.mu.Unlock()

	cfg := e.cfg.ErrorQueue
	query := e.errorQueueQuery()
	e.beginQueueRun(QueueError)
	n, err := e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueFailedTask())
	e.endQueueRun(QueueError, n, err)
	// 本轮到点即采样一次积压，监控页无需等下一轮采样周期
	e.sampleQueueBacklogOne(QueueError, query)
	return n, err
}

// requeueFailedTask 将单条失败任务重置为 pending 并重新投递到其阶段工作池。
func (e *Engine) requeueFailedTask() func(*models.CrawlerTask) bool {
	return e.requeueFailedTaskIn
}

// requeueFailedTaskIn 单条重投的实体。
func (e *Engine) requeueFailedTaskIn(t *models.CrawlerTask) bool {
	// 站点也要看：阶段名只在本站内唯一，光按名字查会把另一站的同名阶段当成本站那个。
	if e.stages[stageKeyOfRecord(t)] == nil {
		e.loggerSet.Engine.Warnf("error queue: stage %s not registered, skip task %d", stageKeyOfRecord(t), t.ID)
		return false
	}
	// **`status` 守卫不能省**：批次 SELECT 与这次重置之间，**轮询队列**可能已经把这一行捞走并重投
	//（一条 `repeatable + failed` 的行同时命中两个队列，两者各自有锁、互不互斥）：那一份被 worker
	// 认领、正在跑，而这次无条件 UPDATE 照样把它改回 pending 并再投一份 → 同一行被两个 worker
	// 并发执行。带上 status 之后：已经不是 failed 的行（正在跑 / 已终态）匹配不上，0 行 → 跳过。
	res := e.db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ?", t.ID, models.TaskStatusFailed).
		Updates(map[string]any{
			"status":    models.TaskStatusPending,
			"retry":     0,
			"reprocess": gorm.Expr("reprocess + 1"),
		})
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("error queue: reset task %d: %s", t.ID, res.Error.Error())
		return false
	}
	if res.RowsAffected == 0 {
		return false // 行没了 / 已不是 failed：跳过，不计数也不标失败
	}

	task := e.taskFromRecord(t)
	task.Retry = 0 // 上面刚把行里的 retry 归零，内存副本跟上，别让 handler 读到已经作废的重试次数
	e.dedupCache.Add(task.Unique())
	if err := e.submitToStage(task); err != nil {
		if errors.Is(err, workerpool.ErrQueueFull) {
			// 队列达高水位：**保持 pending**（上面刚重置过）、塞进溢出列表由 drain 每 2s 回灌。
			// 否则行被标成 failed + `retry + 1`，而重置时**已经**把 `reprocess + 1` 写进去了。
			// 队列自己的判据是 `reprocess < max_retry`，几轮之后这些行 `reprocess` 到顶，
			// **永久不再被错误队列捞起**，只能人工点「重投」。
			e.spilledCount.Add(1)
			e.spillTask(task)
		} else {
			e.dedupCache.Delete(task.Unique())
			e.loggerSet.Engine.Errorf("error queue: submit task %d: %s", t.ID, err.Error())
			// 上面刚把这行重置成 pending，失败不管的话它就再也没人捞了（本队列只捞 failed）
			e.markRequeueFailed(QueueError, t.ID, err)
			return false
		}
	}
	e.errorQueue.retried.Add(1)
	e.emit(events.Event{
		Type: events.TypeTaskRequeued, Queue: QueueError, Source: "auto",
		Site: t.Site, Stage: t.Stage, TaskID: t.ID, Reprocess: t.Reprocess + 1,
	})
	return true
}

// errorRetriedTotal 累计重新投递数之和（监控埋点 `error_retry_total`）。
func (e *Engine) errorRetriedTotal() int64 {
	if e.errorQueue == nil {
		return 0
	}
	return e.errorQueue.retried.Load()
}

// startErrorQueue 启动错误队列定时自动处理（全局一条 ticker）。
func (e *Engine) startErrorQueue() {
	e.runDynamicTicker(func() time.Duration {
		cfg := e.cfg.ErrorQueue
		if !cfg.Enabled || cfg.Interval <= 0 {
			return 0
		}
		return cfg.Interval
	}, nil /* 数据驱动的唤醒只有轮询队列用得上 */, func() {
		if n, err := e.ProcessErrorQueue(); err != nil {
			e.loggerSet.Engine.Errorf("error queue: auto process: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("error queue: auto processed %d failed tasks", n)
		}
	})
}
