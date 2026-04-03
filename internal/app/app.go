package app

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/fetcher"
	"github.com/ydtg1993/papa/internal/loggers"
	"github.com/ydtg1993/papa/internal/models"
	"github.com/ydtg1993/papa/internal/server"
	"github.com/ydtg1993/papa/pkg/browser"
	"github.com/ydtg1993/papa/pkg/database"
	"github.com/ydtg1993/papa/pkg/monitor"
	"gorm.io/gorm"
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
	loggerSet := loggers.NewLoggerSet(cfg.Log)

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

	// 5. 初始化浏览器池
	browserPool, err := browser.NewPool(browser.PoolConfig{
		Size:        cfg.Browser.PoolSize,
		MaxIdleTime: cfg.Browser.MaxIdleTime,
		Headless:    cfg.Browser.Headless,
		NoSandbox:   cfg.Browser.NoSandbox,
		BrowserPath: cfg.Browser.BrowserPath,
		Flags:       map[string]string{},
		DefaultHeaders: map[string]string{
			"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
			"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
		},
		// 如果需要代理，设置 ProxyManager
	})
	if err != nil {
		return nil, fmt.Errorf("new browser pool: %w", err)
	}

	// 6. 创建爬虫引擎（注入依赖）
	engine := crawler.NewEngine(browserPool, db, cfg, &loggerSet)

	// 7. 注册阶段
	if err := registerStages(engine, cfg, loggerSet.Engine); err != nil {
		return nil, fmt.Errorf("register stages: %w", err)
	}

	return &App{
		Config:      cfg,
		Logger:      &loggerSet,
		DB:          db,
		BrowserPool: browserPool,
		Engine:      engine,
	}, nil
}

// registerStages 专门负责注册各个爬取阶段，避免 NewApp 过长
func registerStages(engine *crawler.Engine, cfg *config.Config, logger *logrus.Logger) error {
	// 从配置中读取 stage 配置
	catalogCfg := cfg.Crawler.Stages["catalog"]
	fetchCatalog := &fetcher.FetchCatalog{}
	if err := engine.RegisterStage(fetchCatalog, crawler.StageConfig{
		WorkerCount: catalogCfg.WorkerCount,
		QueueSize:   catalogCfg.QueueSize,
		NextStage:   "detail",
		Handler:     fetchCatalog.FetchHandler,
	}); err != nil {
		return fmt.Errorf("register catalog stage: %w", err)
	}

	// 提交起始任务
	if err := engine.SubmitTask(&crawler.Task{
		URL:   cfg.Crawler.Target,
		Stage: fetchCatalog.GetStage(),
	}); err != nil {
		logger.Errorf("submit initial task: %v", err)
	}

	// 启动错误监听
	go func() {
		for err := range engine.Errors("catalog") {
			logger.Errorf("catalog error: %v", err)
		}
	}()

	return nil
}

// Run 启动引擎，等待退出信号
func (a *App) Run(ctx context.Context) {
	// 启动监控 HTTP 服务（如果配置启用）
	if a.Config.Monitor.Enabled {
		getter := func() map[string]*monitor.Monitor[*crawler.Task] {
			return a.Engine.GetMonitors()
		}
		// 使用 a.Logger.Sys 作为日志（需实现 monitor.Logger 接口，或简单适配）
		mServer := server.NewMonitor(a.Config.Monitor.Port, getter, a.Logger.Sys)
		go mServer.Start(ctx)
	}
	// 恢复未完成任务
	a.Engine.RecoverTasks()
	//触发结束任务 清理资源
	<-ctx.Done()
	a.Logger.Sys.Info("shutdown signal received, stopping engine...")
	a.Engine.Stop(5 * time.Second)
	if a.BrowserPool != nil {
		a.BrowserPool.Close()
	}
	if sqlDB, err := a.DB.DB(); err == nil {
		_ = sqlDB.Close()
	}
	a.Logger.Sys.Info("shutdown completed")
}
