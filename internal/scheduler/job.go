package scheduler

import (
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/models"
	"gorm.io/gorm"
	"time"
)

type SchJob struct {
	engine *crawler.Engine
	logger *logrus.Logger
}

func (s *SchJob) Init(sched *Scheduler) {
	s.engine = sched.engine
	s.logger = sched.logger
}

// RepeatJob 周期性执行 catalog 阶段，检查新增任务
type RepeatJob struct {
	SchJob
}

func NewRepeatJob(sched *Scheduler) *RepeatJob {
	rj := &RepeatJob{}
	rj.Init(sched) // 显式调用基类初始化
	return rj
}

func (r *RepeatJob) Run() {
	r.logger.Info("catalog repeat job started")
	r.engine.GetRepeatTasks().Range(func(k, v interface{}) bool {
		task := v.(crawler.Task)
		task.UpdateStatus(r.engine.GetDB(), r.logger, models.TaskStatusPending, nil)
		if err := r.engine.SubmitTask(&task); err != nil {
			r.logger.Errorf("repeat job submit failed: %v", err)
		} else {
			r.logger.Infof("repeat job submitted task: %v", task)
		}
		return true
	})
}

// RecoverJob 定期恢复未完成的任务
type RecoverJob struct {
	SchJob
}

func NewRecoverJob(sched *Scheduler) *RecoverJob {
	rj := &RecoverJob{}
	rj.Init(sched) // 显式调用基类初始化
	return rj
}

func (j *RecoverJob) Run() {
	e := j.engine
	// 1. 查询需要恢复的任务（pending或processing超时的）
	var tasks []models.CrawlerTask
	timeout := time.Now().Add(-6 * time.Hour)

	if err := e.GetDB().Where("(status = ? OR status = ?) AND updated_at < ?",
		models.TaskStatusPending, models.TaskStatusProcessing, timeout).
		Find(&tasks).Error; err != nil {
		j.logger.Errorf("failed to load tasks for recovery: %v", err)
		return
	}

	if len(tasks) == 0 {
		j.logger.Info("no tasks need recovery")
		return
	}

	j.logger.Infof("found %d tasks need to recover", len(tasks))

	// 2. 分别记录提交成功和失败的任务 ID
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
		e.DelActiveTask(task)
		// 重新提交到队列（非阻塞）
		task.UpdateStatus(j.engine.GetDB(), j.logger, models.TaskStatusPending, nil)
		if err := e.SubmitTask(task); err != nil {
			j.logger.Errorf("recover task %d (url: %s) submit failed: %v",
				t.ID, t.URL, err)
			failIDs = append(failIDs, t.ID)
		}
	}

	// 4. 失败的任务状态回滚为 Pending（记录为失败进入失败任务流程）
	if len(failIDs) > 0 {
		if err := e.GetDB().Model(&models.CrawlerTask{}).
			Where("id IN ?", failIDs).
			Updates(map[string]interface{}{
				"status": models.TaskStatusFailed,
				"retry":  gorm.Expr("retry + 1"),
				"error":  gorm.Expr("CONCAT(COALESCE(error, ''), ?)", "RecoverTasks 恢复任务提交队列失败\n"),
			}).Error; err != nil {
			j.logger.Errorf("failed to rollback failed tasks to failed: %v", err)
		} else {
			j.logger.Warnf("rolled back %d tasks to pending due to submit failure", len(failIDs))
		}
	}
}
