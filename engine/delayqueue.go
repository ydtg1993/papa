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
	if !e.deliveryState().AcquireForStage(task.ID, stageKeyOf(task)) {
		return
	}
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

// deliverDelayed 到点后重读状态，再通知阶段恢复；不阻塞全局计时器。
func (e *Engine) deliverDelayed(item *delayedTask) {
	var record models.CrawlerTask
	err := e.db.Model(&models.CrawlerTask{}).Where("id = ?", item.task.ID).First(&record).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		e.deliveryState().Release(item.task.ID)
		return // 行已被删：没有可投的东西
	case err != nil:
		// 瞬态错误（库抖动）：**不能丢**（行还是 pending，丢了就再没人投它），
		// 也不能立刻重排（会退化成忙等）—— 短暂退避后再看一次。
		e.loggerSet.Engine.Warnf("delayed task %d: reread failed, will retry: %s", item.task.ID, err.Error())
		e.requeueDelayedAfter(item, time.Second)
		return
	}
	if record.Status != models.TaskStatusPending {
		e.deliveryState().Release(item.task.ID)
		// 终态 = 已经跑完/被标失败；processing = 已经有一份在跑（再投就是同一行并发跑两遍）
		e.loggerSet.Engine.Infof("delayed task %d dropped: status is no longer pending", item.task.ID)
		return
	}
	// 到期后转交阶段恢复协程，避免阻塞全局延迟计时器。
	e.deliveryState().Release(item.task.ID)
	e.requestPendingRecovery(item.task)
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
