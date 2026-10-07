package scheduler

import (
	"context"
	"fmt"
	"github.com/robfig/cron/v3"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/engine"
	"time"
	// 内嵌时区库：alpine/distroless/scratch 等基础镜像不带 /usr/share/zoneinfo，
	// 不引入则 LoadLocation("Asia/Shanghai") 必失败，容器内启动即崩。
	_ "time/tzdata"
)

type Scheduler struct {
	cron   *cron.Cron
	engine *engine.Engine
	logger *logrus.Logger
	jobs   map[string]cron.EntryID
}

// NewScheduler 造一个调度器。参数叫 eng 而不是 engine：包名就是 engine，
// 形参同名会把包名遮在函数体里，`engine: engine` 这种写法读起来像打错了。
func NewScheduler(eng *engine.Engine, logger *logrus.Logger, timezone string) *Scheduler {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		panic(err)
	}
	return &Scheduler{
		cron:   cron.New(cron.WithSeconds(), cron.WithLocation(location)), // 支持秒级
		engine: eng,
		logger: logger,
		jobs:   make(map[string]cron.EntryID),
	}
}

// AddJob 添加一个计划任务
func (s *Scheduler) AddJob(name, spec string, cmd func()) error {
	entryID, err := s.cron.AddFunc(spec, cmd)
	if err != nil {
		return fmt.Errorf("add job %s: %w", name, err)
	}
	s.jobs[name] = entryID
	s.logger.Infof("scheduler: added job %s with schedule %s", name, spec)
	return nil
}

// RemoveJob 动态移除任务
func (s *Scheduler) RemoveJob(name string) {
	if id, ok := s.jobs[name]; ok {
		s.cron.Remove(id)
		delete(s.jobs, name)
		s.logger.Infof("scheduler: removed job %s", name)
	}
}

// Start 启动调度器（非阻塞）
func (s *Scheduler) Start() {
	s.cron.Start()
	s.logger.Info("scheduler started")
}

// Stop 优雅停止（等待正在执行的任务完成）
func (s *Scheduler) Stop() context.Context {
	ctx := s.cron.Stop()
	s.logger.Info("scheduler stopping...")
	return ctx
}
