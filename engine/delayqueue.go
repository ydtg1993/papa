package engine

import (
	"container/heap"
	"errors"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// delayedTask 延迟投递队列中的一个待投递任务。
type delayedTask struct {
	at     time.Time
	task   *Task
	record models.CrawlerTask
}

// delayHeap 按 at 升序的最小堆。
type delayHeap []*delayedTask

func (h delayHeap) Len() int           { return len(h) }
func (h delayHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h delayHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *delayHeap) Push(x any)        { *h = append(*h, x.(*delayedTask)) }
func (h *delayHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return x
}

// enqueueDelayed 将任务加入延迟投递队列，由后台 dispatcher 到点统一入队。
func (e *Engine) enqueueDelayed(at time.Time, task *Task, record models.CrawlerTask) {
	e.delayMu.Lock()
	heap.Push(&e.delayHeap, &delayedTask{at: at, task: task, record: record})
	e.delayMu.Unlock()
	select {
	case e.delayCh <- struct{}{}:
	default:
	}
}

// delayDispatcher 单个后台协程消费延迟队列，到点后入队，避免每个延迟任务各占一个 goroutine。
func (e *Engine) delayDispatcher() {
	for {
		e.delayMu.Lock()
		if e.delayHeap.Len() == 0 {
			e.delayMu.Unlock()
			select {
			case <-e.ctx.Done():
				return
			case <-e.delayCh:
			}
			continue
		}
		wait := time.Until(e.delayHeap[0].at)
		e.delayMu.Unlock()

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-e.ctx.Done():
				timer.Stop()
				return
			case <-e.delayCh: // 有更早的任务到达，重新计算
				timer.Stop()
				continue
			case <-timer.C:
			}
		}

		e.delayMu.Lock()
		if e.delayHeap.Len() == 0 {
			e.delayMu.Unlock()
			continue
		}
		item := heap.Pop(&e.delayHeap).(*delayedTask)
		e.delayMu.Unlock()

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
	var record models.CrawlerTask
	err := e.db.Model(&models.CrawlerTask{}).Where("id = ?", item.task.ID).First(&record).Error
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
	if record.Status != models.TaskStatusPending {
		// 终态 = 已经跑完/被标失败；processing = 已经有一份在跑（再投就是同一行并发跑两遍）
		e.loggerSet.Engine.Infof("delayed task %d dropped: status is no longer pending", item.task.ID)
		return
	}
	if err := e.submitToPool(item.task, record); err != nil {
		e.loggerSet.Engine.Errorf("delayed submit task %d failed: %s", item.task.ID, err.Error())
	}
}

// requeueDelayedAfter 把一条延迟任务放回堆里、改到 after 之后再投（只给"重读失败"那条路用）。
func (e *Engine) requeueDelayedAfter(item *delayedTask, after time.Duration) {
	e.delayMu.Lock()
	item.at = time.Now().Add(after)
	heap.Push(&e.delayHeap, item)
	e.delayMu.Unlock()
	select {
	case e.delayCh <- struct{}{}:
	default:
	}
}

// drainInterval 回灌/重试用的间隔（与 startDrain 同一个配置项与兜底）。
func (e *Engine) drainInterval() time.Duration {
	if interval := e.cfg.Crawler.DrainInterval; interval > 0 {
		return interval
	}
	return 2 * time.Second
}
