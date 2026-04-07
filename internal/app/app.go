package app

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/models"
	"github.com/ydtg1993/papa/internal/scheduler"
	"github.com/ydtg1993/papa/internal/server"
	"github.com/ydtg1993/papa/pkg/browser"
	"github.com/ydtg1993/papa/pkg/database"
	"github.com/ydtg1993/papa/pkg/loggers"
	"github.com/ydtg1993/papa/pkg/middleware"
	"github.com/ydtg1993/papa/pkg/track"
	"gorm.io/gorm"
	"reflect"
	"time"
)

type App struct {
	Config      *config.Config
	Logger      *loggers.LoggerSet // 自定义一个结构，包含各类logger
	DB          *gorm.DB
	BrowserPool *browser.Pool
	Engine      *crawler.Engine
}

// NewApp 统一初始化所有组件，并完成依赖注入
func NewApp() (*App, error) {
	// 1. 加载配置
	cfg, err := config.Load("configs/config.yaml")
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// 2. 初始化日志（传入配置，以便控制日志级别、输出等）
	loggerSet := loggers.NewLoggerSet(loggers.LoggerConfig{
		Dir:        cfg.Log.Dir,
		MaxSize:    cfg.Log.MaxSize,
		MaxAge:     cfg.Log.MaxDays,
		MaxBackups: cfg.Log.MaxBackups,
		LocalTime:  cfg.Log.LocalTime,
		Compress:   cfg.Log.Compress,
	})

	// 3. 初始化数据库
	db, err := database.NewDB(cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	// 4. 自动迁移（开发环境）
	if cfg.App.Env == "dev" {
		if err := database.AutoMigrate(db, &models.CrawlerTask{}); err != nil {
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}

	// 5. 创建爬虫引擎（注入依赖）
	engine := crawler.NewEngine(db, cfg, &loggerSet)

	return &App{
		Config: cfg,
		Logger: &loggerSet,
		DB:     db,
		Engine: engine,
	}, nil
}

// RegisterStage 注册爬虫业务阶段流程
func (a *App) RegisterStage(stage string, NextStage string, fetcher crawler.Fetcher, subFunc func(engine *crawler.Engine)) {
	// 从配置中读取 stage 配置
	cfg, ok := a.Config.Crawler.Stages[stage]
	if ok != true {
		panic(fmt.Errorf("invalid crawler stage: %s", stage))
	}
	if _, ok = a.Config.Crawler.Stages[NextStage]; ok != true && NextStage != "" {
		panic(fmt.Errorf("invalid crawler next stage: %s", NextStage))
	}
	if cfg.WorkerCount <= 0 || cfg.QueueSize <= 0 {
		panic(fmt.Errorf("stage %s: WorkerCount and QueueSize must be positive", stage))
	}
	if cfg.Retry.MaxAttempts <= 0 {
		cfg.Retry.MaxAttempts = 3
	}
	if cfg.Retry.Backoff <= 0 {
		cfg.Retry.Backoff = time.Second
	}

	a.Engine.AddStage(stage, crawler.StageConfig{
		MaxAttempts: cfg.Retry.MaxAttempts,
		Backoff:     cfg.Retry.Backoff,
		NextStage:   NextStage,
		WorkerCount: cfg.WorkerCount,
		QueueSize:   cfg.QueueSize,
	}, fetcher, subFunc)
}

// Run 启动引擎，等待退出信号
func (a *App) Run(ctx context.Context) {
	// 初始化引擎浏览器池
	a.Engine.SetBrowserPool()
	// 启用工作流和对应工作池
	a.Engine.ApplyRegisterStage()

	// 任务计划
	sch := a.schedule()
	// 监听c错误日志 中间件活动等队列消息
	a.mdMsgListener(ctx)
	// 启动监控 HTTP 服务（如果配置启用）
	mon := a.monitor()

	//触发结束任务 清理资源
	<-ctx.Done()
	a.Logger.Sys.Info("shutdown signal received, stopping engine...")
	if sch != nil {
		sch.Stop()
	}
	if mon != nil {
		mon.Stop()
	}
	a.Engine.Stop(5 * time.Second)
	if reflect.ValueOf(a.Engine.GetBrowserPool()).IsNil() == false {
		a.Engine.GetBrowserPool().Close()
	}
	if sqlDB, err := a.DB.DB(); err == nil {
		_ = sqlDB.Close()
	}
	a.Logger.Sys.Info("shutdown completed")
}

// mdMsgListener 监听中间件活动日志和错误
func (a *App) mdMsgListener(ctx context.Context) {
	//监听引擎各stage的workerpool池消息
	for _, e := range a.Engine.Errors() {
		go func() {
			for err := range e {
				a.Logger.Engine.Errorf("engine error: %v", err)
			}
		}()
	}
	//监听中间件消息
	listen := func(md middleware.Err, log *logrus.Logger) {
		if reflect.ValueOf(md).IsNil() {
			return
		}
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case err, ok := <-md.GetErrors():
					if !ok {
						return // 通道关闭
					}
					log.Error(err)
				}
			}
		}()
	}
	listen(a.Engine.GetProxy(), a.Logger.Proxy)
	listen(a.Engine.GetFiledown(), a.Logger.Filedown)
	listen(a.Engine.GetM3U8(), a.Logger.M3U8)
}

// monitor
func (a *App) monitor() *server.Monitor {
	if a.Config.Monitor.Enabled == false {
		return nil
	}
	getter := func() map[string]*track.StatsQueue[*crawler.Task] {
		return a.Engine.GetStatsQueue()
	}
	// 使用 a.Logger.Sys 作为日志（需实现 monitor.Logger 接口，或简单适配）
	mServer := server.NewMonitor(a.Config.Monitor.Port, getter, a.Logger.Sys)
	go mServer.Start()
	return mServer
}

// 任务计划
func (a *App) schedule() *scheduler.Scheduler {
	if a.Config.Scheduler.Enabled == false {
		return nil
	}
	sched := scheduler.NewScheduler(a.Engine, a.Config.Scheduler.Timezone)
	for _, jobCfg := range a.Config.Scheduler.Jobs {
		var cmd func()
		switch jobCfg.Type {
		case "repeat":
			repeatJob := scheduler.NewRepeatJob(a.Engine)
			cmd = repeatJob.Run
		case "recover":
			recoverJob := scheduler.NewRecoverJob(a.Engine)
			cmd = recoverJob.Run
		default:
			panic(fmt.Sprintf("unknown job type: %s", jobCfg.Type))
		}

		if err := sched.AddJob(jobCfg.Name, jobCfg.Schedule, cmd); err != nil {
			a.Engine.LoggerSet.Scheduler.Errorf("failed to add job %s: %v", jobCfg.Name, err)
		}
	}
	go sched.Start()
	return sched
}
