package app

import (
	"context"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/admin/oplog"
	"github.com/ydtg1993/papa/v4/admin/sysinfo"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/reportrunner"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/pkg/account/onboarding"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"gorm.io/gorm"
	"net/http"
	"sync"
)

type App struct {
	Config      *config.Config
	Logger      *loggers.LoggerSet // 自定义一个结构，包含各类logger
	DB          *gorm.DB
	BrowserPool *browser.Pool
	Engine      *engine.Engine

	configPath  string
	extraModels []any
	// events 运行事件流（默认关）。业务侧通过 App.Events() 复用同一个 sink，
	// 于是框架事件与业务事件写在同一个 events.jsonl、共享 seq/run_id。
	events events.Emitter
	runID  string
	// 报告扩展点（Run 之前注册）：业务段/业务模式。papa 只编排与渲染，不认识其语义。
	reportSections []obs.Section
	reportPatterns []obs.Pattern
	// 报告定时生成（obs.report.interval）：Run 期间按间隔刷新「当天」日报。
	// 组件负责取消、等待与串行生成（都发生在关库之前），避免
	// 定时报告与退出报告，避免同一天目录被两个 goroutine 同时写。
	reportOnce      sync.Once
	reportRunner    *reportrunner.Runner
	tables          []oao.Table
	pages           []Page          // 业务注册的自定义页
	routers         []func(*Router) // 业务注册的自定义路由组（UseRouter）
	extraJS         []string        // UseScript / UsePage 注入的 JS，拼成 /static/custom.js
	extraCSS        []string        // UseCSS 注入的样式，拼成 /static/custom.css
	sysInfo         *sysinfo.Collector
	httpSrv         *http.Server // 优雅关停时要先停它，见 shutdownHTTP
	httpRequests    *requestDrain
	whitelistReload *access.WhitelistReloader
	oplog           *oplog.Recorder // 操作日志的异步写入器；关停时要先排空再关库
	cancel          context.CancelFunc
	// configStore 运行期配置的读写口（crawler_config）：白名单与站点快照共用**同一个**实例 ——
	// 它带一层内存缓存（见 admin/configstore 的 cache.go），两份数据共用一把锁、各自一个键。
	configStore       *configstore.Store
	runtimeState      runtimeState
	identityState     browserIdentityState
	accountOnboarding *onboarding.Service
	messageListeners  messageListenerState

	shutdownHooks []func(context.Context) error // Run 前注册，HTTP 退出后、关库前执行
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
