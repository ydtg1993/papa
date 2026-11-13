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
			e.spillTask(task)
			return
		}
	}
	// A probe already admitted by the breaker carries a process-local deadline;
	// its token must stay resident through every retry/delay transfer.
	if e.residents != nil && at.After(time.Now().Add(e.schedulerLimits().DelayWindow)) && task.breakerEpoch == 0 && task.breakerTrial == 0 && task.breakerSampleDeadline.IsZero() {
		e.residents.release(task, "")
		return
	}

	if !e.residents.admit(task, "delay") {
		return
	}
	if !e.delays.PushUnique(spillIdentity(task), at, &delayedTask{at: at, task: task, record: record}) {
		e.residents.release(task, "delay")
	}
}
func (e *Engine) delayDispatcher() {
	for {
		item, ok := e.delays.Next(e.ctx)
		if !ok {
			return
		}
		e.deliverDelayed(item)
	}
}

// deliverDelayed 到点后投递一条延迟任务。
//
// **不能拿入队那一刻的 record 直接投**：`Delay` 有多长，那份快照就有多旧，而到点前运营随时可能
// 动过这一行 ——「重投」并跑完（success）、手动标失败（failed）、或已被别的路径投出去并被 worker
// 认领（processing）。照旧快照投的后果是 `submitUpdates` 把它无条件写回 pending，紧接着
// `claimTask`（要的正是 pending）必定认领成功 → 已经跑完的任务被**再跑一遍**（覆盖 content、
// 重复下载、重复写业务），或者"标失败"这个拦截手段被静默绕过去。
//
// 所以到点先重读行，**只有仍是 pending 才投**（与 `drainSpilled` 对溢出任务的处理同一套：
// 重读、遇终态即丢）。多投一份的代价远大于晚投一次。
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
		// 也不能立刻重排（会退化成忙等）—— 隔一个回灌间隔再看一次。
		e.loggerSet.Engine.Warnf("delayed task %d: reread failed, will retry: %s", item.task.ID, err.Error())
		e.requeueDelayedAfter(item, e.drainInterval())
		return
	}
	if record.Status != models.TaskStatusPending || item.task.staleDelivery(record) {
		// 终态 = 已经跑完/被标失败；processing = 已经有一份在跑（再投就是同一行并发跑两遍）
		e.loggerSet.Engine.Infof("delayed task %d dropped: status is no longer pending", item.task.ID)
		return
	}
	// 投不进去（池刚被回收 / 已停机）时 submitToPool 会把它放进溢出表，回灌那一跳重投
	e.submitToPool(item.task, record)
}

// requeueDelayedAfter 把一条延迟任务放回堆里、改到 after 之后再投（只给"重读失败"那条路用）。
func (e *Engine) requeueDelayedAfter(item *delayedTask, after time.Duration) {
	item.at = time.Now().Add(after)
	e.residents.move(item.task, "delay")
	if !e.delays.PushUnique(spillIdentity(item.task), item.at, item) {
		e.residents.release(item.task, "delay")
	}
}

// drainInterval 回灌/重试用的间隔（与 startDrain 同一个配置项与兜底）。
func (e *Engine) drainInterval() time.Duration {
	if interval := e.cfg.Crawler.DrainInterval; interval > 0 {
		return interval
	}
	return 2 * time.Second
}
