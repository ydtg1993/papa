package engine

import (
	"errors"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// spillBacklog 返回当前溢出列表中待回灌的任务总数。
func (e *Engine) spillBacklog() int {
	e.spillMu.Lock()
	defer e.spillMu.Unlock()
	n := 0
	for _, tasks := range e.spilled {
		n += len(tasks)
	}
	return n
}

// spillTasks 整桶放回溢出表（一次锁）。逐条 spillTask 会按条数反复取锁 ——
// 暂停的桶每 drain_interval 都要原样放回一次，那条路要便宜。
func (e *Engine) spillTasks(key string, tasks []*Task) {
	if len(tasks) == 0 {
		return
	}
	e.spillMu.Lock()
	e.spilled[key] = append(e.spilled[key], tasks...)
	e.spillMu.Unlock()
}

// spillTask 将任务加入高水位溢出列表，等待 drain 重新入队。
// 桶按**阶段键**（站点 + 阶段名）分：两个站的同名阶段各有各的溢出队列，不互相占位。
func (e *Engine) spillTask(task *Task) {
	key := stageKeyOf(task)
	e.spillMu.Lock()
	e.spilled[key] = append(e.spilled[key], task)
	e.spillMu.Unlock()
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
	e.spillMu.Lock()
	if len(e.spilled) == 0 {
		e.spillMu.Unlock()
		return
	}
	all := e.spilled
	e.spilled = make(map[string][]*Task)
	e.spillMu.Unlock()

	for key, tasks := range all {
		info := e.stages[key]
		if info == nil {
			continue
		}
		// 这个 scope 正被暂停：**整桶放回，连库都不查**。
		// 这是本次改动的实际收益：以前闸住的站，这里每 drain_interval（默认 2s）对每条溢出任务
		// 做一次 SELECT + 一次 UPDATE，而入队又判队满 → 再溢出，永远推进不了。
		// 恢复时 ResumeSite 会 wakeDrainNow()，所以寄放的任务不会等到下一个 tick。
		if e.isPaused(pauseScope{Site: info.config.Site, Stage: info.stage}) {
			e.spillTasks(key, tasks)
			continue
		}
		for _, task := range tasks {
			var record models.CrawlerTask
			err := e.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record).Error
			if err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					// 瞬态错误：重新加入溢出列表，下轮重试
					e.spillTask(task)
				}
				continue
			}
			if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
				continue // 已终态，无需再入队
			}
			if err := e.submitToPool(task, record); err != nil {
				e.loggerSet.Engine.Errorf("drain spilled task %d: %s", task.ID, err.Error())
			}
		}
	}
}
