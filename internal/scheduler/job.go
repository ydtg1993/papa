package scheduler

import (
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/crawler"
)

// CatalogJob 周期性执行 catalog 阶段，检查新增任务
type CatalogJob struct {
	engine    *crawler.Engine
	targetURL string // 可以从配置或参数传入
	logger    *logrus.Logger
}

func NewCatalogJob(engine *crawler.Engine, targetURL string) *CatalogJob {
	return &CatalogJob{
		engine:    engine,
		targetURL: targetURL,
		logger:    engine.LoggerSet.Scheduler,
	}
}

func (j *CatalogJob) Run() {
	j.logger.Info("catalog job started")
	// 创建一个新的 catalog 任务
	task := &crawler.Task{
		URL:   j.targetURL,
		Stage: "catalog",
		Retry: 0,
	}
	// 提交任务（非阻塞，内部会写入数据库并加入队列）
	if err := j.engine.SubmitTask(task); err != nil {
		j.logger.Errorf("catalog job submit failed: %v", err)
	} else {
		j.logger.Infof("catalog job submitted task: %s", task.URL)
	}
}

// RecoverJob 定期恢复未完成的任务
type RecoverJob struct {
	engine *crawler.Engine
}

func NewRecoverJob(engine *crawler.Engine) *RecoverJob {
	return &RecoverJob{engine: engine}
}

func (j *RecoverJob) Run() {
	j.engine.RecoverTasks()
}
