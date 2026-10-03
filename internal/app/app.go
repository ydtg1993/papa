package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/crawler"
	"github.com/ydtg1993/papa/v2/internal/auth"
	"github.com/ydtg1993/papa/v2/internal/database"
	"github.com/ydtg1993/papa/v2/internal/oplog"
	"github.com/ydtg1993/papa/v2/internal/scheduler"
	"github.com/ydtg1993/papa/v2/internal/server"
	"github.com/ydtg1993/papa/v2/internal/sysinfo"
	"github.com/ydtg1993/papa/v2/internal/tasksource"
	"github.com/ydtg1993/papa/v2/internal/tokenadmin"
	"github.com/ydtg1993/papa/v2/models"
	"github.com/ydtg1993/papa/v2/pkg/browser"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
	"github.com/ydtg1993/papa/v2/pkg/middleware"
	"gorm.io/gorm"
	"net/http"
	"os"
	"path/filepath"
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
	runtimePath string
	extraModels []any
	tables      []oao.Table
	pages       []Page   // 业务注册的自定义页
	extraJS     []string // UseScript / UsePage 注入的 JS，拼成 /static/custom.js
	extraCSS    []string // UseCSS 注入的样式，拼成 /static/custom.css
	sysInfo     *sysinfo.Collector
	cancel      context.CancelFunc
	customJobs  []cronJob // 业务注册的自定义定时任务
}

// cronJob 业务注册的自定义定时任务。
type cronJob struct {
	name     string
	schedule string
	fn       func()
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

// UseTables 注册监控后台的表格页（须在 Run 之前调用 —— 路由在 Run 时挂载）。
// 表格声明与数据来源见组件库 github.com/ydtg1993/oao：业务声明"显示什么、
// 怎么显示"并实现 oao.Source 提供数据，框架不碰数据层。
// 放在 New 之后是为了能用 app.DB 构造 Source。
func (a *App) UseTables(tables ...oao.Table) {
	a.tables = append(a.tables, tables...)
}

// Page 一个自定义后台页：清单进侧边栏，渲染由 Script 提供。
// Script 是**受信任代码**，在里面调 Papa.page(Key, fn) 注册渲染函数：
//
//	app.UsePage(papa.Page{
//	    Key: "review", Label: "审核", Group: "业务",
//	    Script: `Papa.page("review", function (el, meta) {
//	        el.innerHTML = "<h2>" + esc(meta.label) + "</h2>";   // Toast/Dialog/apiFetch 等都是全局可用的
//	    });`,
//	})
type Page struct {
	Key    string // 菜单与 DOM 标识：字母、数字、下划线、连字符
	Label  string // 菜单文案，留空用 Key
	Group  string // 侧边栏分组，留空归 "General"
	Script string // 受信任 JS，必须调 Papa.page(Key, fn) 注册渲染
}

// UsePage 注册一个自定义页（须在 Run 之前调用 —— 路由在 Run 时挂载）。
// 声明有问题直接 panic：与 RegisterStage 同风格，启动即失败，别等点了菜单才发现。
func (a *App) UsePage(p Page) {
	if !validPageKey(p.Key) {
		panic(fmt.Errorf("invalid page key %q: 只允许字母、数字、下划线、连字符", p.Key))
	}
	for _, x := range a.pages {
		if x.Key == p.Key {
			panic(fmt.Errorf("duplicate page key %q", p.Key))
		}
	}
	if strings.TrimSpace(p.Script) == "" {
		panic(fmt.Errorf("page %q: Script 为空 —— 里面要调 Papa.page(%q, fn) 注册渲染", p.Key, p.Key))
	}
	a.pages = append(a.pages, p)
	a.extraJS = append(a.extraJS, p.Script)
}

// UseScript 往后台页注入一段受信任 JS（拼进 /static/custom.js）。
// 它能用的全局件见 docs/MONITOR.md：Toast / Dialog / skeletonRows / esc / apiFetch / apiPost 等。
//
// 注意：注入内容与 mo.js / oao.js 一样走**免鉴权**静态路由（浏览器标签没法带自定义头），
// 所以别把密钥、令牌之类写进去 —— 数据接口仍在白名单 + 密钥后面。
func (a *App) UseScript(js string) {
	if strings.TrimSpace(js) == "" {
		return
	}
	a.extraJS = append(a.extraJS, js)
}

// UseScriptFile 读一个 .js 文件注入；读不到直接 panic（部署问题，不静默）。
func (a *App) UseScriptFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Errorf("UseScriptFile %s: %w", path, err))
	}
	a.UseScript(string(b))
}

// UseCSS 注入一段样式（拼进 /static/custom.css）。它在 mo.css / oao.css 之后加载，可以覆盖。
func (a *App) UseCSS(css string) {
	if strings.TrimSpace(css) == "" {
		return
	}
	a.extraCSS = append(a.extraCSS, css)
}

// UseCSSFile 读一个 .css 文件注入；读不到直接 panic。
func (a *App) UseCSSFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Errorf("UseCSSFile %s: %w", path, err))
	}
	a.UseCSS(string(b))
}

// validPageKey 页面 key 会进 DOM 与菜单标识，限制成 URL/DOM 安全字符（与 oao 的表 key 同一套规则）。
func validPageKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// operatorOf 从操作事件带的请求里取「操作人」：身份是鉴权中间件（internal/auth）解析访问令牌后
// 写进请求上下文的。宿主自己构造事件时 Req 可能为 nil —— 那就记空，不影响审计写库。
func operatorOf(ev oao.ActionEvent) string {
	if ev.Req == nil {
		return ""
	}
	return auth.OperatorFrom(ev.Req.Context())
}

// mountCustomRoutes 挂自定义页与注入的三条路由（在监控后台的装配里调用）。
//
//   - 清单走 /api/pages，和白名单 + 密钥一起校验（菜单是数据）；
//   - 脚本/样式走 /static/，与 oao.js 同一条**免鉴权**静态路由 —— 浏览器 <script src>/<link>
//     没法带自定义头，而注入内容按"非机密"对待（见 UseScript 注释）。
//
// 三条路由**无条件注册**（没有内容时返回空体 / 空清单），这样 template.html 里的两个标签是静态的，
// 宿主没注入东西也不会 404。抽成独立方法是为了能在没有数据库时单测（完整装配需要 MySQL）。
func (a *App) mountCustomRoutes(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	pageList := make([]map[string]string, 0, len(a.pages))
	for _, p := range a.pages {
		label, group := p.Label, p.Group
		if label == "" {
			label = p.Key
		}
		if group == "" {
			group = "General"
		}
		pageList = append(pageList, map[string]string{"key": p.Key, "label": label, "group": group})
	}
	jsBody := strings.Join(a.extraJS, "\n;\n")
	cssBody := strings.Join(a.extraCSS, "\n")

	mux.Handle("/static/custom.js", server.NoCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(jsBody))
	})))
	mux.Handle("/static/custom.css", server.NoCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(cssBody))
	})))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"pages": pageList})
	})
	if wrap != nil {
		mux.Handle("/api/pages", wrap(handler))
	} else {
		mux.Handle("/api/pages", handler)
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
		// 访问令牌表与审计表一样，是框架自己要用的，不依赖业务开关
		allModels := []any{&models.CrawlerTask{}, &models.AccessToken{}}
		if cfg.Server.OperationLog {
			allModels = append(allModels, &models.OperationLog{})
		}
		if cfg.Crawler.Trace.Enabled {
			allModels = append(allModels, &models.TaskTrace{})
		}
		allModels = append(allModels, a.extraModels...)
		if err := database.AutoMigrate(db, allModels...); err != nil {
			return nil, fmt.Errorf("migrate db: %w", err)
		}
	}

	// 4.1 步骤追踪开着但表不存在 = 每次尝试写库都失败。生产不会自动迁移，所以这里必须显式喊一声，
	// 不能重演审计表那种「开关看着是开的、实际什么都没记」的静默丢失。
	if cfg.Crawler.Trace.Enabled && !db.Migrator().HasTable(&models.TaskTrace{}) {
		loggerSet.Sys.Errorf("crawler.trace.enabled 已开启，但表 crawler_task_trace 不存在 —— " +
			"步骤追踪不会写入任何数据。生产环境不做自动迁移，请手工建表（结构见 models.TaskTrace）；" +
			"dev 环境重启即自动创建")
	}

	// 5. 创建爬虫引擎
	engine := crawler.NewEngine(db, cfg, &loggerSet)

	// 5.1 加载运行期覆盖（runtime.yaml）并应用到引擎；文件缺失视为空覆盖
	runtimePath := filepath.Join(filepath.Dir(cfgPath), "runtime.yaml")
	runtimeCfg, err := config.LoadRuntime(runtimePath)
	if err != nil {
		loggerSet.Sys.Errorf("load runtime config %s: %s", runtimePath, err.Error())
		runtimeCfg = &config.RuntimeConfig{}
	}
	if err := engine.ApplyRuntimeConfig(runtimeCfg); err != nil {
		return nil, fmt.Errorf("apply runtime config: %w", err)
	}

	a.Config = cfg
	a.Logger = &loggerSet
	a.DB = db
	a.Engine = engine
	a.runtimePath = runtimePath
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
	if cfg.Delay.Min <= 0 {
		cfg.Delay = config.DurationRange{Min: time.Minute, Max: time.Minute}
	}

	a.Engine.AddStage(stage, crawler.StageConfig{
		MaxAttempts: cfg.Retry.MaxAttempts,
		Backoff:     cfg.Retry.Backoff,
		WorkerCount: cfg.WorkerCount,
		QueueSize:   cfg.QueueSize,
		Delay:       cfg.Delay,
	}, fetcher, subFunc)
}

// RegisterCronJob 注册一个业务自定义定时任务（cron 表达式，支持秒级，如 "0 3 * * * *"）。
// 必须在 Run 之前调用；任务随框架生命周期一起启动与优雅停止。
// fn 内可通过闭包访问 app 的 Engine/DB 等资源。
func (a *App) RegisterCronJob(name, schedule string, fn func()) {
	a.customJobs = append(a.customJobs, cronJob{name: name, schedule: schedule, fn: fn})
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
	// 关停时把运行期覆盖层落盘（delta 写回 runtime.yaml，重启后叠加生效）
	if a.runtimePath != "" {
		if err := config.SaveRuntime(a.runtimePath, a.Engine.GetRuntimeConfig()); err != nil {
			a.Logger.Sys.Errorf("save runtime config: %s", err.Error())
		}
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
		getter := func() map[string]crawler.StageStats {
			return a.Engine.GetStageStats()
		}
		if a.sysInfo == nil {
			a.sysInfo = sysinfo.NewCollector(2*time.Second, cfg.MonitorDirs)
			a.sysInfo.Start(ctx)
		}
		whitelist := a.resolveWhitelist(cfg)
		// 访问令牌：库表里多条、每条属于一个操作人（原来配置里的单 auth_key 已废弃）
		auth.WarnIfNoToken(a.DB, a.Logger.Sys)
		mon := server.NewMonitor(getter, a.Logger.Sys, server.MonitorConfig{
			VerifyToken:         auth.Verifier(a.DB, a.Logger.Sys),
			Whitelist:           whitelist,
			WhitelistFile:       cfg.WhitelistFile,
			Metrics:             a.Engine.GetMetrics,
			QueueStats:          a.Engine.GetQueueStats,
			SysInfo:             a.sysInfo,
			LogDir:              a.Config.Log.Dir,
			OnShutdown:          a.Shutdown,
			ProcessErrorQueue:   a.Engine.ProcessErrorQueue,
			ProcessRecoverQueue: a.Engine.ProcessRecoverQueue,
			ProcessRepeatQueue:  a.Engine.RepollRepeatableTasks,
			ConfigGet:           a.Engine.GetRuntimeConfig,
			ConfigSet:           a.Engine.ApplyRuntimeConfig,
			TaskTrace:           a.Engine.ListTrace,
		})
		mon.Register(mux)

		// 操作日志：开启时，oao 表格页的动作与后台自带的「访问令牌」页都往这里记。
		var rec *oplog.Recorder
		if cfg.OperationLog {
			rec = oplog.New(a.DB, a.Logger.Sys)
		}

		// 表格组件：内置「任务」表 + 业务用 UseTables 注册的表。
		// 它不碰数据层，只把请求转给各自的 Source；写操作转给业务 Handler。
		tables := append([]oao.Table{tasksource.Table(a.DB, a.Engine)}, a.tables...)
		cfgOao := oao.Config{
			Tables: tables,
			Logger: a.Logger.Sys,
			Auth:   mon.Auth, // 复用监控后台的白名单 + 令牌校验
		}
		if rec != nil {
			cfgOao.OnAction = func(ev oao.ActionEvent) { rec.Record(ev, operatorOf(ev)) }
			cfgOao.Tables = append(tables, oplog.Table(a.DB))
		}
		o, err := oao.New(cfgOao)
		if err != nil {
			a.Logger.Sys.Errorf("init table component: %s", err.Error())
		} else {
			o.Mount(mux)
			if sub, err := o.StaticFS(); err == nil {
				mux.Handle("/static/oao/", http.StripPrefix("/static/oao/",
					server.NoCache(http.FileServer(http.FS(sub)))))
			} else {
				a.Logger.Sys.Errorf("oao static fs: %s", err.Error())
			}
		}

		// 「访问令牌」页是后台自带的模块，不走表格组件 —— 它要「新增」，
		// 而表格组件的动作只回 {"status":"ok"}，没法把服务端生成的明文令牌交给操作人。
		// 写操作与表格页一样记进操作日志（用条件更新防重复点击，见 internal/tokenadmin）。
		var tokenHook tokenadmin.Hook
		if rec != nil {
			tokenHook = func(ev tokenadmin.Event) {
				rec.RecordEvent(oplog.Event{
					Table: ev.Table, Action: ev.Action,
					RowID:    strconv.FormatUint(uint64(ev.ID), 10),
					Values:   ev.Values,
					Err:      ev.Err,
					IP:       ev.IP,
					Operator: auth.OperatorFrom(ev.Req.Context()),
					At:       ev.At,
				})
			}
		}
		tokenadmin.NewAPI(tokenadmin.NewStore(a.DB), tokenHook, a.Logger.Sys).Register(mux, mon.Auth)

		// 自定义页与注入（阶段 3 的逃生舱）
		a.mountCustomRoutes(mux, mon.Auth)
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
	if len(a.customJobs) == 0 {
		return
	}
	sched := scheduler.NewScheduler(a.Engine, a.Logger.Scheduler, a.Config.Scheduler.Timezone)
	for _, job := range a.customJobs {
		if err := sched.AddJob(job.name, job.schedule, job.fn); err != nil {
			a.Logger.Scheduler.Errorf("failed to add custom job %s: %s", job.name, err.Error())
		}
	}
	go sched.Start()
	go func() {
		<-ctx.Done()
		sched.Stop()
	}()
}
