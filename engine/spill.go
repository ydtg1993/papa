package engine

import (
	"errors"
	"time"

	"github.com/ydtg1993/papa/v2/models"
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

// spillTask 将任务加入高水位溢出列表，等待 drain 重新入队。
func (e *Engine) spillTask(task *Task) {
	e.spillMu.Lock()
	e.spilled[task.Stage] = append(e.spilled[task.Stage], task)
	e.spillMu.Unlock()
}

// startDrain 启动后台回灌协程：定期把溢出列表中的任务重新入队。
func (e *Engine) startDrain() {
	interval := e.cfg.Crawler.DrainInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
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

	for stage, tasks := range all {
		info := e.stages[stage]
		if info == nil || info.workerPool == nil {
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
