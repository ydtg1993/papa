package crawler

import "github.com/ydtg1993/papa/v2/models"

// RepollRepeatableTasks 重新投递所有 repeatable 任务，返回实际投递数量。
// 供业务层通过 RegisterCronJob 周期性调用，实现「轮询任务」的定时重跑（原内置 repeat job 已移除）。
func (e *Engine) RepollRepeatableTasks() (int, error) {
	var tasks []models.CrawlerTask
	if err := e.db.Where("repeatable = ?", models.RepeatableYes).Find(&tasks).Error; err != nil {
		return 0, err
	}
	var n int
	for _, t := range tasks {
		task := &Task{
			ID:             int(t.ID),
			PID:            int(t.PID),
			URL:            t.URL,
			Stage:          t.Stage,
			Repeatable:     true,
			IdempotencyKey: t.IdempotencyKey,
		}
		task.UpdateStatus(e.db, models.TaskStatusPending, nil)
		if err := e.SubmitTask(task); err != nil {
			e.loggerSet.Engine.Errorf("repoll task %d: %s", t.ID, err.Error())
			continue
		}
		n++
	}
	return n, nil
}
