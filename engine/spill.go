package engine

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// spillBacklog 返回当前溢出列表中待回灌的任务总数。
func (e *Engine) spillBacklog() int { return e.overflow.Count() }

// spillTasks 整桶放回溢出表（一次锁）。逐条 spillTask 会按条数反复取锁 ——
// 暂停的桶每 drain_interval 都要原样放回一次，那条路要便宜。
func (e *Engine) spillTasks(key string, tasks []*Task) {
	for _, t := range tasks {
		e.spillTask(t)
	}
}

// spillTask 将任务加入高水位溢出列表，等待 drain 重新入队。
// 桶按**阶段键**（站点 + 阶段名）分：两个站的同名阶段各有各的溢出队列，不互相占位。
//
// 去重：同一个任务（同 ID；ID 为 0 时退化成 Unique）只放一份。append 不去重的话，
// 同一任务从多条投递路径（队满 / 池被回收）反复 spill 会越积越多，drain 对同一条行重复 SELECT。
func (e *Engine) spillTask(task *Task) {
	if e.residents.admit(task, "spill") {
		if old := e.overflow.Put(task); old != nil && old != task {
			e.residents.release(task, "")
		}
	}
}

// startDrain 启动后台回灌协程：定期把溢出列表中的任务重新入队。
func (e *Engine) startDrain() {
	interval := e.drainInterval()
	// 进 tickerWG：停止时要等这一跳跑完 —— 它中途写库，调用方看到 drained 就去关库的话，
	// 那些写入会全部撞 "database is closed"（而且这条路径的错误本来就不检查，一行日志都没有）。
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-e.wakeDrain:
				// 有人放行了（ResumeSite / ResumeStage / ResumeCrawling）：立刻把寄放的任务灌回去，
				// 别让"恢复"等下一个 tick —— drain_interval 可以配到小时。
				e.drainSpilled()
			case <-ticker.C:
				e.drainSpilled()
			}
		}
	}()
}

// drainSpilled 把溢出列表中队列已有空间的任务重新入队；仍满则继续留在列表。
func (e *Engine) drainSpilled() {
	limits := e.schedulerLimits()
	e.refillMu.Lock()
	defer e.refillMu.Unlock()
	e.offloadPausedDelays(limits.RefillBatch)
	buckets := len(e.overflow.Counts())
	submitted := 0
	deadline := time.Now().Add(100 * time.Millisecond)
	for b := 0; b < buckets && time.Now().Before(deadline); b++ {
		if e.queueContext().Err() != nil {
			break
		}
		key, tasks := e.overflow.TakeBatch(limits.RefillBatch)
		if len(tasks) == 0 {
			break
		}
		info := e.stages[key]
		if info == nil {
			for _, t := range tasks {
				e.residents.release(t, "")
			}
			continue
		}
		if e.isPaused(pauseScope{Site: info.config.Site, Stage: info.stage}) {
			for _, task := range tasks {
				if e.residents != nil && task.deliveryDurable && task.ID != 0 && task.breakerEpoch == 0 && task.breakerTrial == 0 && task.breakerSampleDeadline.IsZero() {
					e.residents.release(task, "")
				} else {
					e.spillTask(task)
				}
			}
			continue
		}
		ids := make([]int, 0, len(tasks))
		for _, task := range tasks {
			ids = append(ids, task.ID)
		}
		batchCtx, cancel := context.WithDeadline(e.queueContext(), deadline)
		rows, err := taskstore.LoadSchedulingBatch(batchCtx, e.db, ids)
		cancel()
		if err != nil {
			e.spillTasks(key, tasks)
			continue
		}
		records := make(map[int]models.CrawlerTask, len(rows))
		for _, row := range rows {
			records[int(row.ID)] = row
		}
		for i, task := range tasks {
			if e.queueContext().Err() != nil || time.Now().After(deadline) {
				e.spillTasks(key, tasks[i:])
				break
			}
			record, exists := records[task.ID]
			if !exists {
				e.residents.release(task, "")
				continue
			}

			if record.Status != models.TaskStatusPending || task.staleDelivery(record) {
				e.residents.release(task, "")
				continue
			}
			e.submitToPool(task, record)
			submitted++
			if e.residents.state(task) == "spill" {
				e.spillTasks(key, tasks[i+1:])
				break
			}
		}
	}
	e.refillPendingLocked(deadline)
	if submitted > 0 {
		e.emit(events.Event{Type: events.TypeSpillChanged, Source: "drained", Counts: map[string]int{"drained": submitted}, Backlog: int64(e.spillBacklog())})
	}
}
