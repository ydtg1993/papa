package scheduler

import (
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/models"
	"time"
)

// RepeatJob 周期性执行 catalog 阶段，检查新增任务
type RepeatJob struct {
	engine *crawler.Engine
	logger *logrus.Logger
}

func NewRepeatJob(engine *crawler.Engine) *RepeatJob {
	return &RepeatJob{
		engine: engine,
		logger: engine.LoggerSet.Scheduler,
	}
}

func (r *RepeatJob) Run() {
	r.logger.Info("catalog repeat job started")
	r.engine.RepeatTasks.Range(func(k, v interface{}) bool {
		task := v.(crawler.Task)
		if err := r.engine.SubmitTask(&task, false); err != nil {
			r.logger.Errorf("repeat job submit failed: %v", err)
		} else {
			r.logger.Infof("repeat job submitted task: %v", task)
		}
		return true
	})
}

// RecoverJob 定期恢复未完成的任务
type RecoverJob struct {
	engine *crawler.Engine
}

func NewRecoverJob(engine *crawler.Engine) *RecoverJob {
	return &RecoverJob{engine: engine}
}

func (j *RecoverJob) Run() {
	e := j.engine
	// 1. 查询需要恢复的任务（pending 或超时的 processing）
	var tasks []models.CrawlerTask
	timeout := time.Now().Add(-2 * time.Hour)

	if err := e.DB.Where("(status = ? OR status = ?) AND updated_at < ?",
		models.TaskStatusPending, models.TaskStatusProcessing, timeout).
		Find(&tasks).Error; err != nil {
		e.LoggerSet.DB.Errorf("failed to load tasks for recovery: %v", err)
		return
	}

	if len(tasks) == 0 {
		e.LoggerSet.Engine.Info("no tasks need recovery")
		return
	}

	e.LoggerSet.Engine.Infof("found %d tasks need to recover", len(tasks))

	// 2. 分别记录提交成功和失败的任务 ID
	var successIDs []uint
	var failIDs []uint
	for _, t := range tasks {
		task := &crawler.Task{
			ID:         int(t.ID),
			PID:        int(t.PID),
			URL:        t.URL,
			Stage:      t.Stage,
			Retry:      t.Retry,
			Repeatable: false,
		}
		// 剔除去重hash map的暂存
		e.ActiveTasks.Delete(task.Unique())
		// 重新提交到队列（非阻塞）
		if err := e.SubmitTask(task, false); err != nil {
			e.LoggerSet.Engine.Errorf("recover task %d (url: %s) submit failed: %v",
				t.ID, t.URL, err)
			failIDs = append(failIDs, t.ID)
		} else {
			successIDs = append(successIDs, t.ID)
			e.LoggerSet.Engine.Infof("recover task %d submitted successfully", t.ID)
		}
	}

	// 3. 批量更新成功任务的状态为 Processing
	if len(successIDs) > 0 {
		if err := e.DB.Model(&models.CrawlerTask{}).
			Where("id IN ?", successIDs).
			Update("status", models.TaskStatusProcessing).Error; err != nil {
			e.LoggerSet.DB.Errorf("failed to mark recovered tasks as processing: %v", err)
		} else {
			e.LoggerSet.Engine.Infof("marked %d tasks as processing", len(successIDs))
		}
	}

	// 4. 失败的任务状态回滚为 Pending（记录为失败进入失败任务流程）
	if len(failIDs) > 0 {
		if err := e.DB.Model(&models.CrawlerTask{}).
			Where("id IN ?", failIDs).
			Update("status", models.TaskStatusFailed).
			Update("error", "RecoverTasks 恢复任务提交队列失败").Error; err != nil {
			e.LoggerSet.DB.Errorf("failed to rollback failed tasks to failed: %v", err)
		} else {
			e.LoggerSet.Engine.Warnf("rolled back %d tasks to pending due to submit failure", len(failIDs))
		}
	}
}
