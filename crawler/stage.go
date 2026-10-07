package crawler

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/internal/track"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
)

// AddNotifier 注册告警通知器；任务最终失败时触发 Notify。
func (e *Engine) AddNotifier(n Notifier) {
	if n == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifiers = append(e.notifiers, n)
}

func (e *Engine) getNotifiers() []Notifier {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Notifier, len(e.notifiers))
	copy(out, e.notifiers)
	return out
}

// notifyFailure 结构化记录失败日志，并触发告警通知器。
func (e *Engine) notifyFailure(ctx context.Context, task *Task, err error) {
	te := TaskError{
		Stage:   task.Stage,
		TaskID:  task.ID,
		URL:     task.URL,
		Retry:   task.Retry,
		Kind:    ErrorKind(err),
		Message: err.Error(),
	}
	e.loggerSet.Engine.WithFields(logrus.Fields{
		"stage":   te.Stage,
		"task_id": te.TaskID,
		"url":     te.URL,
		"retry":   te.Retry,
		"kind":    te.Kind,
	}).Errorf("task failed: %s", te.Message)

	notifiers := e.getNotifiers()
	if len(notifiers) == 0 {
		return
	}
	level := AlertError
	if Retryable(err) {
		level = AlertWarn
	}
	event := AlertEvent{Level: level, TaskError: te}
	for _, n := range notifiers {
		if nerr := n.Notify(ctx, event); nerr != nil {
			e.loggerSet.Engine.Errorf("notify alert failed: %s", nerr.Error())
		}
	}
}

// ApplyRegisterStage 启用注册业务流程开启对应工作池
func (e *Engine) ApplyRegisterStage() {
	for stage, stageInfo := range e.stages {
		cfg := stageInfo.config
		pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize, e.cfg.Crawler.QueueWatermark)
		e.stages[stage].workerPool = pool
		// 启动 worker pool
		pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
			// 认领：把「待处理」置为「处理中」，让运营侧能看出这条归谁管，
			// 也让三个后台动作的状态守卫成立。拿不到行就跳过执行 —— 原因不止一种：
			// 运营已经动过它（标失败/删除），或同一行的另一份副本（后台「加急」会另投一份）先认领了。
			claimed, err := e.claimTask(task)
			if err != nil {
				e.loggerSet.Engine.Errorf("claim task %d: %s", task.ID, err.Error())
			} else if !claimed {
				e.loggerSet.Engine.Warnf("task %d 已被认领或改动（不再是待处理），跳过本次执行", task.ID)
				return nil
			}
			// 重试FetchHandler
			var lastErr error
			for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
				if attempt > 0 {
					task.IncRetry(e.db)
				}
				err := e.runAttempt(ctx, stageInfo.fetcher, task, attempt)
				if err == nil {
					task.UpdateStatus(e.db, models.TaskStatusSuccess, nil)
					if !task.Repeatable {
						e.DelActiveTask(task)
					}
					<-time.After(cfg.Delay.Random())
					return nil
				}
				// 不可重试的错误：直接标 failed，不再空转重试
				if !Retryable(err) {
					task.UpdateStatus(e.db, models.TaskStatusFailed, err)
					e.notifyFailure(ctx, task, err)
					if !task.Repeatable {
						e.DelActiveTask(task)
					}
					return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, err)
				}
				lastErr = err
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(cfg.Backoff * (1 << uint(attempt))):
					continue
				}
			}
			// 所有重试失败：记录错误并更新状态为 failed
			task.UpdateStatus(e.db, models.TaskStatusFailed, lastErr)
			e.notifyFailure(ctx, task, lastErr)
			if !task.Repeatable {
				e.DelActiveTask(task)
			}
			return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, lastErr)
		})
		// 检查提交任务
		if stageInfo.submitFunc != nil {
			stageInfo.submitFunc(e)
		}
		// HTTP 服务开启时，为该阶段创建统计器并启动（数据供监控页面的阶段概览用）
		if e.cfg.Server.Enabled {
			stats := track.NewStatsQueue(pool)
			stats.Start(e.ctx)
			e.setStatsQueue(stage, stats)
			e.loggerSet.Monitor.Infof("monitor started for stage: %s", stage)
		}
	}
	// 启动高水位溢出任务的回灌协程
	e.startDrain()
	// 启动错误队列后台自动轮询（未配置 interval 则不启动，仅手动触发）
	e.startErrorQueue()
	// 启动中断恢复队列（启用时启动即恢复一次 + 定时轮询）
	e.startRecoverQueue()
	// 启动周期轮询队列（repeatable 任务的定时重跑）
	e.startRepeatQueue()
	// 启动步骤追踪的保留期清理（追踪未开启时不启动）
	e.startTraceCleanup()
	// HTTP 服务开启时低频采样三队列积压（COUNT 查询，不进监控页请求路径）
	if e.cfg.Server.Enabled {
		e.startQueueSampler()
	}
}

// runAttempt 执行一次 FetchHandler，并负责这一次尝试的步骤追踪：
// 挂记录器 → 跑 handler → 记下结局 → 落库。
//
// 落库放在 defer 里，所以 panic 展开时也会执行 —— 闭包这一层的 defer 先于 workerpool
// 的 recover 跑，panic 之前已上报的步骤因此不会丢；panic 路径上 setResult 没被调用，
// flush 按失败处理、data 保留，正好是排查现场要看的。
// 这里**不 recover**：panic 继续抛给 workerpool，它那边的 debug.Stack() 与 failed
// 计数才是既有行为，重新 panic 反而会丢掉 handler 的栈帧。
func (e *Engine) runAttempt(ctx context.Context, fetcher Fetcher, task *Task, attempt int) error {
	tr := e.newTrace(task, attempt)
	task.Trace = tr
	defer tr.finish() // finish 幂等，正常返回与 panic 展开都走这里

	// 加急记在 trace 的第一步，事后还能看出"这条曾经加急跑过"：
	// claimTask 认领时会把 urgent 列归零（加急是「排队位置」的概念，跑过一次即完成使命），
	// 那张表上就再也看不出它加急过了。只记第一次尝试 —— 同一次执行里的后续重试不是新的加急。
	if attempt == 0 && task.Urgent {
		tr.Step(traceUrgentStep, nil)
	}

	err := fetcher.FetchHandler(ctx, task, e)
	tr.setResult(err)
	return err
}
