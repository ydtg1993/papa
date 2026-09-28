package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/crawler"
	"github.com/ydtg1993/papa/v2/models"
	"github.com/ydtg1993/papa/v2/pkg/browser"
	"github.com/ydtg1993/papa/v2/pkg/database"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
	"github.com/ydtg1993/papa/v2/pkg/metrics"
	"github.com/ydtg1993/papa/v2/pkg/middleware"
	"github.com/ydtg1993/papa/v2/pkg/sysinfo"
	"github.com/ydtg1993/papa/v2/pkg/track"
	"github.com/ydtg1993/papa/v2/scheduler"
	"github.com/ydtg1993/papa/v2/server"
	"gorm.io/gorm"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type App struct {
	Config      *config.Config
	Logger      *loggers.LoggerSet // 自定义一个结构，包含各类logger
	DB          *gorm.DB
	BrowserPool *browser.Pool
	Engine      *crawler.Engine

	configPath  string
	extraModels []any
	metrics     *metrics.Registry
	sysInfo     *sysinfo.Collector
	cancel      context.CancelFunc
}

// Option 应用初始化选项
type Option func(*App) error

// WithConfigPath 指定配置文件路径；缺省依次读 PAPA_CONFIG 环境变量、configs/config.yaml
func WithConfigPath(path string) Option {
	return func(a *App) error {
		a.configPath = path
		return nil
	}
}

// WithModels 追加需要自动迁移的用户模型（框架默认迁移 CrawlerTask）
func WithModels(models ...any) Option {
	return func(a *App) error {
		a.extraModels = append(a.extraModels, models...)
		return nil
	}
}

// NewApp 统一初始化所有组件，并完成依赖注入
func NewApp(opts ...Option) (*App, error) {
	a := &App{}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}

	// 1. 加载配置
	cfgPath := a.configPath
	if cfgPath == "" {
		cfgPath = os.Getenv("PAPA_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "configs/config.yaml"
	}
	cfg, err := config.Load(cfgPath)
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
		allModels := append([]any{&models.CrawlerTask{}}, a.extraModels...)
		if err := database.AutoMigrate(db, allModels...); err != nil {
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}

	// 5. 创建爬虫引擎（注入依赖）
	engine := crawler.NewEngine(db, cfg, &loggerSet)
	metricsReg := metrics.New()
	engine.SetMetrics(metricsReg)

	a.Config = cfg
	a.Logger = &loggerSet
	a.DB = db
	a.Engine = engine
	a.metrics = metricsReg
	return a, nil
}

// RegisterStage 注册爬虫业务阶段流程
func (a *App) RegisterStage(fetcher crawler.Fetcher, subFunc func(engine *crawler.Engine)) {
	stage := fetcher.GetStage()
	// 从配置中读取 stage 配置
	cfg, ok := a.Config.Crawler.Stages[stage]
	if ok != true {
		panic(fmt.Errorf("invalid crawler stage: %s", stage))
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
	if cfg.Delay <= 0 {
		cfg.Delay = time.Minute
	}

	a.Engine.AddStage(stage, crawler.StageConfig{
		MaxAttempts: cfg.Retry.MaxAttempts,
		Backoff:     cfg.Retry.Backoff,
		WorkerCount: cfg.WorkerCount,
		QueueSize:   cfg.QueueSize,
		Delay:       cfg.Delay,
	}, fetcher, subFunc)
}

// Run 启动引擎，等待退出信号
func (a *App) Run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	defer cancel()

	// 初始化引擎浏览器池
	a.Engine.SetBrowserPool()
	// 初始化静态 HTML 抓取客户端（读取请求头与代理配置）
	a.Engine.SetHTMLClient()
	// 启用工作流和对应工作池
	a.Engine.ApplyRegisterStage()

	// 任务计划
	a.schedule(runCtx)
	// 监听c错误日志 中间件活动等队列消息
	a.mdMsgListener(runCtx)
	// 启动统一 HTTP 服务（监控页面）
	a.httpServer(runCtx)

	//触发结束任务 清理资源
	<-runCtx.Done()
	a.Logger.Sys.Info("shutdown signal received, stopping engine...")
	a.Engine.Stop(5 * time.Second)
	if a.Engine.GetBrowserPool() != nil {
		a.Engine.GetBrowserPool().Close()
	}
	if sqlDB, err := a.DB.DB(); err == nil {
		_ = sqlDB.Close()
	}
	a.Logger.Sys.Info("shutdown completed")
}

// Shutdown 触发优雅退出（供监控后台等外部调用）
func (a *App) Shutdown() {
	if a.cancel != nil {
		a.cancel()
	}
}

// mdMsgListener 监听中间件活动日志和错误
func (a *App) mdMsgListener(ctx context.Context) {
	//监听引擎各stage的workerpool池消息
	for _, e := range a.Engine.Errors() {
		go func() {
			for err := range e {
				a.Logger.Engine.Errorf("engine error: %s", err.Error())
			}
		}()
	}
	//监听中间件消息
	listen := func(md middleware.Err, log *logrus.Logger) {
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
	if proxy := a.Engine.GetProxy(); proxy != nil {
		listen(proxy, a.Logger.Proxy)
	}
	if filedown := a.Engine.GetFiledown(); filedown != nil {
		listen(filedown, a.Logger.Filedown)
	}
	if m3u8 := a.Engine.GetM3U8(); m3u8 != nil {
		listen(m3u8, a.Logger.M3U8)
	}
}

// resolveAuthKey 解析监控访问密钥：优先读 auth_key_file，读不到则回退到内联 auth_key
func (a *App) resolveAuthKey(cfg config.ServerConfig) string {
	if cfg.AuthKeyFile != "" {
		if b, err := os.ReadFile(cfg.AuthKeyFile); err == nil {
			if key := strings.TrimSpace(string(b)); key != "" {
				return key
			}
		} else {
			a.Logger.Sys.Errorf("read auth key file %s: %s", cfg.AuthKeyFile, err.Error())
		}
	}
	return cfg.AuthKey
}

// resolveWhitelist 解析白名单：优先读 whitelist_file（文件存在即采用，即使为空），读不到回退内联 whitelist
func (a *App) resolveWhitelist(cfg config.ServerConfig) []string {
	if cfg.WhitelistFile != "" {
		if entries := readWhitelistFile(cfg.WhitelistFile); entries != nil {
			return entries
		}
	}
	return cfg.Whitelist
}

// readWhitelistFile 读取白名单文件：每行一个 IP/CIDR，忽略空行与 # 注释；文件不存在返回 nil
func readWhitelistFile(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// httpServer 启动统一 HTTP 服务（监控页面）
func (a *App) httpServer(ctx context.Context) {
	cfg := a.Config.Server
	if !cfg.Enabled {
		return
	}

	mux := http.NewServeMux()
	if cfg.Monitor {
		getter := func() map[string]*track.StatsQueue[*crawler.Task] {
			return a.Engine.GetStatsQueue()
		}
		if a.sysInfo == nil {
			a.sysInfo = sysinfo.NewCollector(2*time.Second, cfg.MonitorDirs)
			a.sysInfo.Start(ctx)
		}
		authKey := a.resolveAuthKey(cfg)
		whitelist := a.resolveWhitelist(cfg)
		mon := server.NewMonitor(getter, a.Logger.Sys, server.MonitorConfig{
			AuthKey:       authKey,
			AuthKeyFile:   cfg.AuthKeyFile,
			Whitelist:     whitelist,
			WhitelistFile: cfg.WhitelistFile,
			Metrics:       a.metrics,
			SysInfo:       a.sysInfo,
			LogDir:        a.Config.Log.Dir,
			OnShutdown:    a.Shutdown,
		})
		mon.Register(mux)
	}

	srv := &http.Server{Addr: ":" + strconv.Itoa(cfg.Port), Handler: mux}
	a.Logger.Sys.Infof("http server starting on :%d (monitor=%t)", cfg.Port, cfg.Monitor)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Logger.Sys.Errorf("http server failed: %s", err.Error())
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
}

// 任务计划
func (a *App) schedule(ctx context.Context) {
	if a.Config.Scheduler.Enabled == false {
		return
	}
	sched := scheduler.NewScheduler(a.Engine, a.Logger.Scheduler, a.Config.Scheduler.Timezone)
	for _, jobCfg := range a.Config.Scheduler.Jobs {
		var cmd func()
		switch jobCfg.Type {
		case "repeat":
			repeatJob := scheduler.NewRepeatJob(sched)
			cmd = repeatJob.Run
		case "recover":
			recoverJob := scheduler.NewRecoverJob(sched)
			cmd = recoverJob.Run
		default:
			panic(fmt.Sprintf("unknown job type: %s", jobCfg.Type))
		}

		if err := sched.AddJob(jobCfg.Name, jobCfg.Schedule, cmd); err != nil {
			a.Logger.Scheduler.Errorf("failed to add job %s: %s", jobCfg.Name, err.Error())
		}
	}
	go sched.Start()
	go func() {
		<-ctx.Done()
		sched.Stop()
	}()
}
