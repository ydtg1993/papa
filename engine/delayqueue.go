package engine

import (
	"container/heap"
	"time"

	"github.com/ydtg1993/papa/v2/models"
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

		if err := e.submitToPool(item.task, item.record); err != nil {
			e.loggerSet.Engine.Errorf("delayed submit task %d failed: %s", item.task.ID, err.Error())
		}
	}
}
