package engine

import (
	"errors"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// delayedTask 延迟投递队列中的一个待投递任务。
type delayedTask struct {
	at     time.Time
	task   *Task
	record models.CrawlerTask
}

// enqueueDelayed delegates heap and wake ownership to the scheduler.
func (e *Engine) enqueueDelayed(at time.Time, task *Task, record models.CrawlerTask) {
	if e.residents != nil && e.residents.state(task) == "delay" {
		return
	}
	// Preserve unpublished submission metadata before handing responsibility to DB.
	if e.residents != nil && e.residents.state(task) == "submission" && len(task.Meta) > 0 {
		if err := e.submissions().preparePending(task, uint(task.ID)); err != nil {
			e.deferPending(task)
			return
		}
	}
	// A probe already admitted by the breaker carries a process-local deadline;
	// its token must stay resident through every retry/delay transfer.
	if e.residents != nil && at.After(time.Now().Add(e.schedulerLimits().DelayWindow)) && task.breakerEpoch == 0 && task.breakerTrial == 0 && task.breakerSampleDeadline.IsZero() {
		e.residents.release(task, "")
		e.requestPendingRecovery(task)
		return
	}

	if !e.residents.admit(task, "delay") {
		return
	}
	if !e.delays.PushUnique(deliveryIdentity(task), at, &delayedTask{at: at, task: task, record: record}) {
		e.residents.release(task, "delay")
	}
}
func (e *Engine) delayDispatcher() {
	e.delays.Dispatch(e.ctx, func(item *delayedTask) string {
		return stageKeyOf(item.task)
	}, e.deliverDelayed)
}

// deliverDelayed rechecks durable state before delivering a delayed task.
func (e *Engine) deliverDelayed(item *delayedTask) {
	e.residents.move(item.task, "detached")
	defer e.residents.release(item.task, "detached")
	var record models.CrawlerTask
	err := e.db.WithContext(e.queueContext()).Model(&models.CrawlerTask{}).Where("id = ?", item.task.ID).First(&record).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return // 行已被删：没有可投的东西
	case err != nil:
		// 瞬态错误（库抖动）：**不能丢**（行还是 pending，丢了就再没人投它），
		// 也不能立刻重排（会退化成忙等）—— 一秒后重试读取。
		e.loggerSet.Engine.Warnf("delayed task %d: reread failed, will retry: %s", item.task.ID, err.Error())
		e.requeueDelayedAfter(item, time.Second)
		return
	}
	if record.Status != models.TaskStatusPending || item.task.staleDelivery(record) {
		// 终态 = 已经跑完/被标失败；processing = 已经有一份在跑（再投就是同一行并发跑两遍）
		e.loggerSet.Engine.Infof("delayed task %d dropped: status is no longer pending", item.task.ID)
		return
	}
	if item.task.breakerEpoch != 0 || item.task.breakerTrial != 0 || !item.task.breakerSampleDeadline.IsZero() {
		if e.isPaused(pauseScope{Site: item.task.Site, Stage: item.task.Stage}) {
			e.requeueDelayedAfter(item, time.Second)
			return
		}
		if err := e.deliverPending(e.queueContext(), item.task); err != nil && e.queueContext().Err() == nil {
			e.requeueDelayedAfter(item, time.Second)
		}
		return
	}
	e.submitToPool(item.task, record)
}

// requeueDelayedAfter 把一条延迟任务放回堆里、改到 after 之后再投（只给"重读失败"那条路用）。
func (e *Engine) requeueDelayedAfter(item *delayedTask, after time.Duration) {
	item.at = time.Now().Add(after)
	e.residents.move(item.task, "delay")
	if !e.delays.PushUnique(deliveryIdentity(item.task), item.at, item) {
		e.residents.release(item.task, "delay")
	}
}
