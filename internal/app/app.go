package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/admin/configstore"
	"github.com/ydtg1993/papa/v3/admin/oplog"
	"github.com/ydtg1993/papa/v3/admin/server"
	"github.com/ydtg1993/papa/v3/admin/sitesource"
	"github.com/ydtg1993/papa/v3/admin/sysinfo"
	"github.com/ydtg1993/papa/v3/admin/tasksource"
	"github.com/ydtg1993/papa/v3/admin/tokenadmin"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"github.com/ydtg1993/papa/v3/pkg/middleware"
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
	Engine      *engine.Engine

	configPath  string
	extraModels []any
	tables      []oao.Table
	pages       []Page          // 业务注册的自定义页
	routers     []func(*Router) // 业务注册的自定义路由组（UseRouter）
	extraJS     []string        // UseScript / UsePage 注入的 JS，拼成 /static/custom.js
	extraCSS    []string        // UseCSS 注入的样式，拼成 /static/custom.css
	sysInfo     *sysinfo.Collector
	httpSrv     *http.Server    // 优雅关停时要先停它，见 shutdownHTTP
	oplog       *oplog.Recorder // 操作日志的异步写入器；关停时要先排空再关库
	cancel      context.CancelFunc
	sites       map[string]core.Site // 站点声明（RegisterSites 填；Site() 取用）
	// configStore 运行期配置的读写口（crawler_config）：白名单与站点快照共用**同一个**实例 ——
	// 它带一层内存缓存（见 admin/configstore 的 cache.go），两份数据共用一把锁、各自一个键。
	configStore *configstore.Store
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

// WithModels 追加需要建表的业务模型。框架自带的表（CrawlerTask 等）不用你登记，
// 清单在 database.FrameworkModels 里。这些模型会在 App.Migrate()（`make migrate`）时一起建。
func WithModels(models ...any) Option {
	return func(a *App) error {
		a.extraModels = append(a.extraModels, models...)
		return nil
	}
}

// UseModels 追加需要建表的业务模型，与 WithModels 等价，区别是可以在 New 之后调用。
// 存在的理由：`papa new` 生成的项目在 monitor.Register(app) 里统一登记（清单来自根 models 包的
// Models()），那时 App 已经建好了。
// 与 Migrate 的关系不变 —— Migrate 读的就是这份清单，所以必须在迁移之前调用。
func (a *App) UseModels(models ...any) {
	a.extraModels = append(a.extraModels, models...)
}

// UseTables 注册监控后台的表格页（须在 Run 之前调用 —— 路由在 Run 时挂载）。
// 表格声明与数据来源见组件库 github.com/ydtg1993/oao：业务声明"显示什么、
// 怎么显示"并实现 oao.Source 提供数据，框架不碰数据层。
// 放在 New 之后是为了能用 app.DB 构造 Source。
func (a *App) UseTables(tables ...oao.Table) {
	a.tables = append(a.tables, tables...)
}

// UseRouter 注册一组挂在后台服务上的业务路由（须在 Run 之前调用 —— 路由在 Run 时挂载）。
//
// 回调里用 Router 声明路径、方法与中间件，handler 就是一个普通的 http.HandlerFunc：
//
//	app.UseRouter(func(r *papa.Router) {
//	    r.Use(myLogging)                        // 业务中间件，注册顺序正序执行
//	    r.Group("/api/review", func(g *papa.Router) {
//	        g.Get("/list", listHandler)         // GET /api/review/list
//	        g.Post("/approve", approveHandler)  // POST /api/review/approve
//	    })
//	})
//
// 这些路由默认和后台内置接口一样过「IP 白名单 + 访问令牌」；必须对外公开的（webhook / OAuth 回调）
// 用 r.NoAuth() 显式声明。路由在 HTTP 服务（server.enabled）开启时才挂载 —— 关掉时不会静默：
// 启动会打一条醒目错误日志说明它们没生效。
//
// fn 为 nil 直接 panic：与 UsePage / RegisterSites 同风格，声明有问题启动即失败。
func (a *App) UseRouter(fn func(*Router)) {
	if fn == nil {
		panic(fmt.Errorf("UseRouter: 回调为 nil —— 里面要用 Router 声明路由"))
	}
	a.routers = append(a.routers, fn)
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
// 声明有问题直接 panic：与 RegisterSites 同风格，启动即失败，别等点了菜单才发现。
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

// operatorOf 从操作事件带的请求里取「操作人」：身份是鉴权中间件（admin/auth）解析访问令牌后
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
func (a *App) mountCustomRoutes(mux *http.ServeMux, wrap Middleware) {
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

// mountRouters 把业务用 UseRouter 注册的路由组挂到后台 mux 上（在监控后台的装配里调用）。
// guard 是后台那套「白名单 + 令牌」中间件，Router 默认给每条业务路由套上，NoAuth() 的组除外。
// 与 mountCustomRoutes 一样抽成独立方法，好在没有数据库时单测。
func (a *App) mountRouters(mux *http.ServeMux, guard Middleware) {
	for _, fn := range a.routers {
		fn(newRouter(mux, guard))
	}
}

// NewApp 统一初始化所有组件，并完成依赖注入
func NewApp(opts ...Option) (*App, error) {
	a := &App{sites: make(map[string]core.Site)}
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
	db, err := database.NewDB(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	// 4. 建表不在这里做。迁移只有一条路：显式跑一次（`papa migrate`，或项目里 `make migrate`
	//    走 App.Migrate）。原来 dev 会随启动自动迁一次 —— 那让"迁移"变成一件不用学的事，
	//    而生产上没人会想起来跑它；两边跑同一件事，才谈得上"dev 练过的就是生产要做的"。
	//    这里只检查：该有的表在不在。

	// 4.1 表不在 = 对应功能看着启用、实际什么都写不进去。必须显式喊一声 ——
	// 审计表那条「开关开着、实际什么都没记」的静默丢失就是这么来的。
	for _, m := range database.FrameworkModels(cfg) {
		if !db.Migrator().HasTable(m.Value) {
			loggerSet.Sys.Errorf("表 %s 不存在 —— 相关功能不会写入任何数据。请先跑迁移：`papa migrate`（或项目 Makefile 里的 migrate）", m.Name)
		}
	}

	// 4.2 该有的唯一索引不在也要喊：表在、索引名不对 = 迁移没跑到那一步（或表被手工改过），
	// 与上面"表不存在"是同一类事 —— 都是"看着在跑、实际不对"。唯一索引缺了，
	// 去重就只剩内存表那一层，重启后同一个 URL 会再插一行。
	if db.Migrator().HasTable(&models.CrawlerTask{}) &&
		!db.Migrator().HasIndex(&models.CrawlerTask{}, "idx_site_stage_url_hash") {
		loggerSet.Sys.Errorf("crawler_tasks 缺唯一索引 idx_site_stage_url_hash（(url_hash, stage, site)）—— " +
			"请先跑迁移：`papa migrate`（或项目 Makefile 里的 migrate）")
	}

	// 5. 创建爬虫引擎
	// 局部变量叫 eng 而不是 engine：包名已经是 engine，`engine := engine.NewEngine(...)`
	// 虽然合法（RHS 在声明语句结束前仍解析到包），但读起来像自己给自己赋值。
	eng := engine.NewEngine(db, cfg, &loggerSet)

	a.Config = cfg
	a.Logger = &loggerSet
	a.DB = db
	a.Engine = eng
	a.configStore = configstore.New(db)
	// 站点快照的落点：引擎按声明重建、暂停/恢复时整块回写，读走内存
	//（`crawler_config` 的 `sites` 键，见 admin/configstore/sites.go 与 engine/sitestat.go）。
	a.Engine.SetSiteStore(a.configStore)
	return a, nil
}

// Migrate 建 / 补表：框架自带的（按配置开关）+ 用 papa.WithModels 注册的业务模型。
//
// 迁移只有这一条路 —— 启动时不自动迁。`papa new` 生成的 main.go 把它接在 `-migrate` 参数上
// （Makefile 里就是 `make migrate`），于是"建表"是显式的一步，dev 和生产跑的是同一件事。
// AutoMigrate 只增不减、幂等，重复跑是安全的。
func (a *App) Migrate() error {
	return database.Migrate(a.DB, a.Config, a.extraModels...)
}

// Run 启动引擎，等待退出信号
func (a *App) Run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	defer cancel()

	// 初始化引擎浏览器池
	a.Engine.SetBrowserPool()
	a.BrowserPool = a.Engine.GetBrowserPool() // 同步给导出字段：外部读 App.BrowserPool 不该一直是 nil
	// 初始化静态 HTML 抓取客户端（读取请求头与代理配置）
	a.Engine.SetHTMLClient()
	// 下载器（文件 / m3u8）：框架按 DefaultConfig 备好**基座** —— 它们没有"全局那层配置面"，
	// 要按站点/阶段各配一份写在声明里。必须在 ApplyRegisterStage 之前：两层覆盖是叠在基座上的。
	a.Engine.InitDownloaders()
	// 启用工作流和对应工作池
	a.Engine.ApplyRegisterStage()

	// 监听c错误日志 中间件活动等队列消息
	a.mdMsgListener(runCtx)
	// 启动统一 HTTP 服务（监控页面）
	a.httpServer(runCtx)

	//触发结束任务 清理资源
	<-runCtx.Done()
	a.Logger.Sys.Info("shutdown signal received, stopping engine...")
	// 顺序要紧：先把 HTTP 停下来、等在途请求（比如日志下载）跑完，再动引擎和数据库。
	// 反过来的话，正在下载日志的请求会在读到大半时被关掉的连接 / 关掉的库打断。
	a.shutdownHTTP()

	// 引擎排空：Engine.Stop 内部先 e.cancel()，再等各阶段 worker 把队列跑完（上限 crawler.stop_timeout）。
	// 各阶段是并发等的，所以这里最多等一个 stopTimeout。
	stopTimeout := a.Config.Crawler.StopTimeoutOrDefault()
	drained, stopStats := a.Engine.Stop(stopTimeout)
	if !drained {
		a.Logger.Sys.Errorf("引擎未在 %s 内排空，仍有任务未跑完 %+v —— 跳过关库与浏览器池关闭，让在途写入完成；"+
			"留在库里的 pending 任务下次启动的启动恢复会重新入队", stopTimeout, stopStats)
	}

	// 审计队列先排空再关库：反过来的话最后几条（包括"优雅退出"这条操作本身）写不进去，
	// 只能落到日志文件里。写失败不阻塞响应是常态，但关停这一下要给它一个收尾的机会。
	if a.oplog != nil {
		a.oplog.Close(3 * time.Second)
	}

	// 没排空时**不关**浏览器池和数据库：pool.Stop 超时后返回，但 Go 杀不掉 goroutine，
	// worker 还在跑 —— 它们可能正持着浏览器、正要把结果写回 DB。这会儿关掉，
	// 等于把「让在途任务跑完」这件事又亲手掐断，而且剩下的每次 UpdateStatus / SaveResult
	// 都会报 "sql: database is closed"（日志刷屏，结果照样丢）。
	// 关库/关浏览器本是礼仪不是必须：进程退出时由 OS 回收连接，Chrome 由 leakless 收掉。
	if drained {
		if a.Engine.GetBrowserPool() != nil {
			a.Engine.GetBrowserPool().Close()
		}
		if sqlDB, err := a.DB.DB(); err == nil {
			_ = sqlDB.Close()
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

// warnIfOpenToAll 生效白名单为空时喊一声。
//
// 空白名单是**合法**配置（脚手架生成的 whitelist 文件初始就只有一行注释），但必须说出来：
// 它和"配了却没生效"从行为上分不出来 —— 而后者正是"后台怎么谁都能打开"这类排查里
// 最难想到的方向。按 Error 级打，与 auth.WarnIfNoToken 一个路数（一个是来源那道门，
// 一个是凭据那道门，两道都空就是完全开放）。
//
// 抽成独立函数是为了能离线断言这条警告确实会发 —— 它的调用点在 httpServer 里，那一步要真库真端口。
func warnIfOpenToAll(log *logrus.Logger, whitelist []string) {
	if len(whitelist) > 0 {
		return
	}
	log.Errorf("警告：生效的来源 IP 白名单为空 —— /monitor 与 /api/* 对**任何来源**开放" +
		"（第二道门是访问令牌）；要限制来源，去后台设置页写 IP/CIDR（存在 crawler_config 表的 ip_whitelist 行）")
}

// whitelistReloadInterval 白名单的重载间隔。
//
// 为什么要定期重载：库是唯一事实来源，而库会被**别人**改 —— 多实例后台（A 实例在设置页改完，
// B 实例要能看见）、运维直接改 SQL 修一行。本进程自己保存的那次是即时生效的（写完就换缓存），
// 这个 ticker 只兜上面两种；30s 是"够快，也不至于拿它当热路径"的量级。
const whitelistReloadInterval = 30 * time.Second

// loadIPWhitelist 从配置存储读一次白名单（启动那一次与定期重载都走这里）。
//
// 生效语义（与 configstore.KeyIPWhitelist 的注释、docs 里那张表逐条对应）：
// **行不存在 / 空数组 / 值解析不出来 → 不限制来源**；数组里有效的那几条 → 只允许这些来源。
// 读失败单独记 Error —— 这一档等于把限制**悄悄放宽了**，日志里必须留下痕迹
// （语义是运营定的：宁可放宽，也不因为一次手改把自己锁在后台外面）。值的形状校验在 store 那层。
func loadIPWhitelist(store *configstore.Store, log *logrus.Logger) []string {
	entries, err := store.LoadStrings(configstore.KeyIPWhitelist)
	if err != nil {
		log.Errorf("来源白名单读不出来，按「不限制来源」继续：%s", err.Error())
		return nil
	}
	return entries
}

// reloadWhitelistLoop 定期把库里的白名单同步进本进程的判定缓存。
func (a *App) reloadWhitelistLoop(ctx context.Context, store *configstore.Store, mon *server.Monitor) {
	ticker := time.NewTicker(whitelistReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mon.SetWhitelist(loadIPWhitelist(store, a.Logger.Sys))
		}
	}
}

// httpServer 启动统一 HTTP 服务（监控后台）
func (a *App) httpServer(ctx context.Context) {
	cfg := a.Config.Server
	if !cfg.Enabled {
		// 服务没起来 = 后台的内容（表格页 / 自定义页 / 自定义路由）一个都不会挂载。
		// 不能静默：「代码写了、开关没开、什么都不报」是最难查的一类问题（同「表不存在」那条日志）。
		if len(a.routers) > 0 {
			a.Logger.Sys.Errorf("已注册 %d 组自定义路由，但 HTTP 服务未启用（server.enabled=false）—— 这些路由不会挂载", len(a.routers))
		}
		return
	}

	mux := http.NewServeMux()

	getter := func() map[string]engine.StageStats {
		return a.Engine.GetStageStats()
	}
	if a.sysInfo == nil {
		a.sysInfo = sysinfo.NewCollector(2*time.Second, cfg.MonitorDirs)
		a.sysInfo.Start(ctx)
	}
	// 来源白名单存在 crawler_config 表里（运行期可在设置页改），不在 config.yaml —— 配置里写旧键
	// （server.whitelist / whitelist_file）会在**配置校验**那一关就拒绝启动：未知键一律拒收。
	whitelistStore := a.configStore // 与站点快照共用同一个读写口（同一份内存缓存）
	if err := whitelistStore.EnsureTable(); err != nil {
		// 表不存在就直接起不来。少了它白名单会**静默失效**（读不到 = 不限制来源），一个字都不报 ——
		// 而"起不来"至少把问题摆在运维面前（报错里点名 `papa migrate`）。
		panic(fmt.Errorf("后台运行期配置表没就绪：%w", err))
	}
	whitelist := loadIPWhitelist(whitelistStore, a.Logger.Sys)
	warnIfOpenToAll(a.Logger.Sys, whitelist)
	// 访问令牌：库表里多条、每条属于一个操作人（原来配置里的单 auth_key 已废弃）
	auth.WarnIfNoToken(a.DB, a.Logger.Sys)

	// 操作日志：开启时，oao 表格页的动作、后台自带的「访问令牌」页、以及熔断放行都往这里记。
	// 它得**先于** MonitorConfig 构造 —— 熔断放行的 OnBreakerResume 回调要闭包住这个 rec。
	var rec *oplog.Recorder
	if cfg.OperationLog {
		rec = oplog.New(a.DB, a.Logger.Sys)
		a.oplog = rec
	}

	mon := server.NewMonitor(getter, a.Logger.Sys, server.MonitorConfig{
		VerifyToken: auth.Verifier(a.DB, a.Logger.Sys),
		Whitelist:   whitelist,
		// 设置页保存白名单 → 落 crawler_config；成功后 admin/server 自己换判定缓存（本进程即时生效）。
		SaveWhitelist: func(entries []string, operator string) error {
			return whitelistStore.Save(configstore.KeyIPWhitelist, entries, operator)
		},
		Metrics:            a.Engine.GetMetrics,
		QueueStats:         a.Engine.GetQueueStats,
		SiteStats:          a.Engine.GetSiteStats,
		SysInfo:            a.sysInfo,
		LogDir:             a.Config.Log.Dir,
		ArchiveDir:         a.Engine.ArchiveDir(),
		OnShutdown:         a.Shutdown,
		ProcessErrorQueue:  a.Engine.ProcessErrorQueue,
		ProcessRepeatQueue: a.Engine.RepollRepeatableTasks,
		// 队列「立即执行」也是人在场的干预（批量重置 + 重投），成败都写审计 ——
		// 与上面熔断放行同一个理由，也与表格动作/令牌页保持一致。
		OnQueueProcess: func(ev server.QueueProcessEvent) {
			if rec == nil {
				return
			}
			rec.RecordEvent(oplog.Event{
				Table:    ev.Queue,
				Action:   "process",
				Values:   map[string]any{"processed": ev.Processed},
				Err:      ev.Err,
				IP:       ev.IP,
				Operator: ev.Operator,
				At:       ev.At,
			})
		},
		TaskTrace:        a.Engine.ListTrace,
		VerifyTokenValue: auth.Confirmer(a.DB, a.Logger.Sys),
		BreakerStatuses:  a.Engine.BreakerStatuses,
		// 暂停/放行的 scope 有两层：`stage` 为空 = 整站，否则只动那一站的某个阶段。
		// 两层写的是**同一张暂停信号表**（见 engine/pause.go），只是 scope 不同。
		ResumeBreaker: func(site, stage string) bool {
			if stage == "" {
				return a.Engine.ResumeSite(site)
			}
			return a.Engine.ResumeStage(site, stage)
		},
		OnBreakerResume: func(operator, site, stage string) {
			// 放行是**人在场的干预**：熔断停了整条抓取线，谁在什么时候放的行必须查得到
			if rec == nil {
				return
			}
			values := map[string]any{
				"note":  "手动放行被暂停的抓取",
				"scope": site, // 哪个站（站点是必填维度，不存在空键）
			}
			if stage != "" {
				values["note"] = "手动放行被暂停的抓取（只放行这个阶段）"
				values["stage"] = stage
			}
			rec.RecordEvent(oplog.Event{
				Table:    "breaker",
				Action:   "resume",
				Values:   values,
				Operator: operator,
				At:       time.Now(),
			})
		},
		PauseBreaker: func(site, stage string) bool {
			// 暂停同样是人**在场的干预**（它停了整站的抓取线），所以 reason 由宿主给死，
			// 后台只管点：操作日志里能看清"是谁在后台按的"。
			const reason = "后台手动暂停"
			if stage == "" {
				return a.Engine.PauseSite(site, reason)
			}
			return a.Engine.PauseStage(site, stage, reason)
		},
		OnBreakerPause: func(operator, site, stage string) {
			if rec == nil {
				return
			}
			values := map[string]any{
				"note":  "后台手动暂停了这一站的抓取",
				"scope": site,
			}
			if stage != "" {
				values["note"] = "后台手动暂停了这一站的某个阶段"
				values["stage"] = stage
			}
			rec.RecordEvent(oplog.Event{
				Table:    "breaker",
				Action:   "pause",
				Values:   values,
				Operator: operator,
				At:       time.Now(),
			})
		},
	})
	mon.Register(mux)
	// 多实例 / 运维直接改 SQL 的收敛：定期把库里的白名单同步进判定缓存
	go a.reloadWhitelistLoop(ctx, whitelistStore, mon)

	// 表格组件：内置「任务」表 + 业务用 UseTables 注册的表。
	// 它不碰数据层，只把请求转给各自的 Source；写操作转给业务 Handler。
	tables := append([]oao.Table{
		tasksource.Table(a.DB, a.Engine),
		sitesource.Table(a.Engine.GetSiteStats), // 站点信息（只读：内存快照，不走 SQL）
	}, a.tables...)
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
	// 写操作与表格页一样记进操作日志（用条件更新防重复点击，见 admin/tokenadmin）。
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
	// 业务自定义路由（UseRouter）：默认套上和上面同一道鉴权，NoAuth() 的组除外
	a.mountRouters(mux, mon.Auth)

	readHeader, read, write, idle, _ := cfg.HTTPTimeouts()
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: readHeader,
		ReadTimeout:       read,
		WriteTimeout:      write, // 默认 0 = 不限：日志下载不能被掐
		IdleTimeout:       idle,
	}
	a.httpSrv = srv
	a.Logger.Sys.Infof("http server starting on :%d", cfg.Port)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Logger.Sys.Errorf("http server failed: %s", err.Error())
		}
	}()
	// 这里**不再**自己关服务：关停要排在「等在途请求跑完 → 停引擎 → 关库」这条链的最前面，
	// 顺序在 Run 里统一走（见 shutdownHTTP）。原来在这里 `<-ctx.Done(); srv.Close()`
	// 会和引擎/数据库的收尾并发，把在途的日志下载直接掐断 —— 与"优雅退出"的说法不符。
}

// shutdownHTTP 优雅停下 HTTP 服务：不再接新请求，等在途的跑完（最多 ShutdownTimeout）。
// 超时就强制 Close，别把关停卡死。
func (a *App) shutdownHTTP() {
	if a.httpSrv == nil {
		return
	}
	_, _, _, _, timeout := a.Config.Server.HTTPTimeouts()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := a.httpSrv.Shutdown(ctx); err != nil {
		a.Logger.Sys.Errorf("http server shutdown: %s（超时，强制关闭）", err.Error())
		_ = a.httpSrv.Close()
	}
}
