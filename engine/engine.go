package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine/internal/execution"
	"github.com/ydtg1993/papa/v4/engine/internal/fetchruntime"
	"github.com/ydtg1993/papa/v4/engine/internal/scheduler"
	"github.com/ydtg1993/papa/v4/internal/track"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"gorm.io/gorm"
)

type Engine struct {
	db        *gorm.DB
	loggerSet *loggers.LoggerSet
	// events 运行事件流（默认关）。nil = 关闭；emit 对 nil 安全。
	// 由 internal/app 按 obs.events 配置组装后注入（SetEventsEmitter），业务也可自定义实现。
	events events.Emitter

	ctx context.Context
	// startedAt 本进程启动时刻，只给启动恢复当截止用（见 recoverQueueQuery）：
	// 「本进程启动之后被写过的行」= 本进程碰过的行 → 恢复不重投它们。
	startedAt time.Time
	cancel    context.CancelFunc
	// Cancelled database state retries remain visible in the shutdown report.
	execution execution.State
	// stages 阶段登记表：**键是 `core.StageKey(site, stage)`**，不是裸的阶段名 ——
	// 阶段名只在本站内唯一（两个站可以各有自己的 catalog）。凡是按阶段取东西的地方都用这个键。
	stages map[string]*stageInfo
	// pools 活池登记表：阶段键 -> 当前活着的池。投递/关停/错误通道都从它取池，
	// 不再散落在 stageInfo.workerPool 上（那是裸指针，池一旦懒建/回收就会并发读写）。
	pools                   *poolRegistry
	mu                      sync.RWMutex
	cfg                     *config.Config
	fetchResources          fetchruntime.Resources
	accountEventUnsubscribe func()
	accountAlerts           accountAlertGate
	statsQueue              map[string]*track.StatsQueue[*Task] // key: core.StageKey 分阶段监控信号
	stageTotals             map[string]stageTotals              // 各阶段累计吞吐（池 churn 不清零）；e.mu 保护
	errorCh                 chan error                          // 常驻错误通道：所有池的错误兜底转发到这里，mdMsgListener 读它
	errorForwardCancel      map[string]*poolForwarder           // 每阶段错误转发协程（停机信号 + 归属池）；e.mu 保护
	dedupCache              *dedupCache                         // 有界去重表（LRU），key: 任务去重键；淘汰条目由 DB 唯一索引兜底

	// 暂停信号：**暂停的唯一定义**（见 pause.go）。三个消费者都读它：
	// 取任务点的闸门、入队点的寄放、后台的横幅/卡片/落库。
	// 锁序是硬的：breakers.breakerMu → pauseMu → (overflow.mu | e.mu)，不许反向；pauseMu 绝不跨 DB 调用。
	pauseMu      sync.Mutex
	paused       map[pauseScope]pauseState // scope -> 现场（整站条目的 Stage 为空）
	pauseChanged chan struct{}             // 每次变更换一个新的并 close 旧的（叫醒所有等闸门的人）
	lastResumed  map[pauseScope]time.Time  // 放行时刻（信号删了之后后台 payload 还要它）
	wakeDrain    chan struct{}             // 容量 1：恢复时叫醒回灌循环，别让它等下一个 tick

	residents        *residentSet
	refillMu         sync.Mutex // serializes drain/refill, including DB I/O and cursor updates
	refillCursor     map[string]uint
	refillStage      int
	schedulerStarted bool // published before startDrain; composition is serialized
	overflow         spillQueue
	spilledCount     atomic.Int64 // 累计溢出任务数（监控埋点）
	recoveredCount   atomic.Int64 // 累计启动恢复任务数（recover_queue，只在启动跑一次）
	// recoverSkippedUnregistered 启动恢复里因"阶段未注册"被跳过的条数：
	// 逐条降级为 Debug，启动后汇总一条 Warn（否则一次恢复能把日志刷几百行）。
	recoverSkippedUnregistered atomic.Int64

	queueRuns     map[string]*queueRunState // 治理队列的运行快照（监控页读取）
	queueCounters map[string]*atomic.Int64  // 队列名 -> 累计重新投递计数

	delays scheduler.Delay[*delayedTask]

	// siteStore 站点快照的落点（内存优先 + 落库投影），由 internal/app 注入 —— 见 sitestat.go。
	// 引擎自己不持有站点数据：那份"内存"在落点的缓存里（configstore 的 `sites` 键），
	// 于是"监控页读内存、不查库"与"库里那份是投影"这两件事只有一份实现。
	siteStore SiteStore

	// errorQueue / repeatQueue 失败重投与周期轮询队列：各**一条全局队列**（不再按站点拆）。
	// 运行态在 NewEngine 里建好，之后只读。
	errorQueue  *errorQueueState
	repeatQueue *repeatQueueState

	recoverQueueMu sync.Mutex // 串行化启动恢复，避免重复投递（ProcessRecoverQueue 是导出的，业务也可能调）
	// tickerWG 动态 ticker 的等待组：Stop 要等它们退出再返回，否则调用方看到 drained=true
	// 就去关库，而在途的那一跳可能正在写库（`sql: database is closed`）。
	tickerWG sync.WaitGroup
	// wakeCoalesce 动态 ticker 的唤醒合并窗口（见 dynticker.go 的 coalesceWake）。
	// 0 = 不合并（测试/手工构造的 Engine 默认行为），生产由 NewEngine 设默认值。
	wakeCoalesce time.Duration

	proxy         *proxy.Manager // 代理管理器中间件
	governor      *governance.Controller
	runtimeDelays map[string]time.Duration
	m3u8          *m3u8.Downloader     // m3u8下载器（全局那份）
	filedown      *filedown.Downloader // 文件下载器（全局那份）
	// 下载覆盖配置及解析快照由 Downloads 持锁发布；Engine 只在装配期提供阶段值。
	downloads fetchruntime.Downloads
	notifiers []Notifier // 告警通知器，任务最终失败时触发
	// One detector and one current lifecycle per site.
	breakers breakerCoordinator
	// sites 站点声明快照（Key/BaseURL），由 App.RegisterSites 填；handler 用 Site(task.Site) 取。
	// 框架只把它当标签，不做任何限制（见 core.Site 的注释）。
	sites siteCatalog
}

// NewEngine 创建引擎
func NewEngine(db *gorm.DB, cfg *config.Config, loggerSet *loggers.LoggerSet) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	engine := &Engine{
		ctx:    ctx,
		cancel: cancel,
		// startedAt 本进程启动时刻：启动恢复（recover queue）拿它当截止 —— 见 recoverQueueQuery。
		// 用应用时钟（time.Now）而不是库时钟，是因为 updated_at 也是 gorm 在应用侧写的
		// （autoUpdateTime），两边同一个钟，不需要校时。
		startedAt:          time.Now(),
		stages:             make(map[string]*stageInfo),
		pools:              newPoolRegistry(),
		dedupCache:         newDedupCache(cfg.Crawler.DedupCacheSize),
		db:                 db,
		cfg:                cfg,
		loggerSet:          loggerSet,
		queueRuns:          newQueueRuns(),
		stageTotals:        make(map[string]stageTotals),
		errorCh:            make(chan error, 100),
		errorForwardCancel: make(map[string]*poolForwarder),
		// 容量 1：恢复时叫醒回灌循环（非阻塞发送，重复叫醒只留一次）
		wakeDrain: make(chan struct{}, 1),
		// 启动恢复会连续 submit（每条都可能 wakeRepeatQueue）：合并窗口把同一批 wake 的
		// N 次节拍重算（MIN 聚合查询）压成一次；见 dynticker.go。
		wakeCoalesce: defaultWakeCoalesce,
	}
	// 熔断计数器不在这里建：每站一个、由声明层按站点建（SetSiteBreaker）。
	// 终态结果进入 Detector；达到保护线之后写暂停信号并发一条
	// AlertCritical（业务不用再接一套告警通道）。
	// 两条治理队列（全局各一条）的运行态 + 累计计数：键就是监控面板上的队列名。
	engine.errorQueue = &errorQueueState{}
	engine.repeatQueue = &repeatQueueState{wake: make(chan struct{}, 1)}
	engine.queueCounters = map[string]*atomic.Int64{
		QueueError:  &engine.errorQueue.retried,
		QueueRepeat: &engine.repeatQueue.repolled,
	}
	engine.residents = newResidentSet(SchedulerLimits{})
	engine.loadActiveTasks()
	// 延迟投递 dispatcher 同样进 tickerWG：它到点时会写库（status=pending），
	// Stop 要等它退出再返回（否则调用方关库时它可能正写到一半）。
	engine.tickerWG.Add(1)
	go func() {
		defer engine.tickerWG.Done()
		engine.delayDispatcher()
	}()
	return engine
}
