package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
	"github.com/ydtg1993/papa/v3/internal/database"
	"github.com/ydtg1993/papa/v3/internal/metrics"
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"github.com/ydtg1993/papa/v3/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
	"gorm.io/gorm"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	db        *gorm.DB
	loggerSet *loggers.LoggerSet

	ctx context.Context
	// startedAt 本进程启动时刻，只给启动恢复当截止用（见 recoverQueueQuery）：
	// 「本进程启动之后被写过的行」= 本进程碰过的行 → 恢复不重投它们。
	startedAt time.Time
	cancel    context.CancelFunc
	// stages 阶段登记表：**键是 `core.StageKey(site, stage)`**，不是裸的阶段名 ——
	// 阶段名只在本站内唯一（两个站可以各有自己的 catalog）。凡是按阶段取东西的地方都用这个键。
	stages map[string]*stageInfo
	// pools 活池登记表：阶段键 -> 当前活着的池。投递/关停/错误通道都从它取池，
	// 不再散落在 stageInfo.workerPool 上（那是裸指针，池一旦懒建/回收就会并发读写）。
	pools              *poolRegistry
	mu                 sync.RWMutex
	cfg                *config.Config
	browserPool        *browser.Pool
	htmlClient         *htmlfetch.Client
	statsQueue         map[string]*track.StatsQueue[*Task] // key: core.StageKey 分阶段监控信号
	stageTotals        map[string]stageTotals              // 各阶段累计吞吐（池 churn 不清零）；e.mu 保护
	errorCh            chan error                          // 常驻错误通道：所有池的错误兜底转发到这里，mdMsgListener 读它
	errorForwardCancel map[string]chan struct{}            // 每阶段错误转发协程停机信号（reap 时 close）；e.mu 保护
	dedupCache         *dedupCache                         // 有界去重表（LRU），key: 任务去重键；淘汰条目由 DB 唯一索引兜底

	// 阶段入口：点火串行化用的锁，与"上次点火时刻"的锁**分开**（见 entry.go 的理由）。
	// entryMu 用 TryLock，不会阻塞后台请求；也绝不能换成 e.mu —— 点火会走到建池，
	// 建池路上要拿 e.mu（setStatsQueue），非重入锁当场自锁。
	entryMu      sync.Mutex
	entryLastMu  sync.Mutex
	entryLast    map[string]time.Time // 阶段键 -> 上次手动点火时刻（零值/缺 = 从未）
	entriesReady atomic.Bool          // ApplyRegisterStage 跑完才置真：之前点火会踩坑（见 entryReadyErr）

	// 暂停信号：**暂停的唯一定义**（见 pause.go）。三个消费者都读它：
	// 取任务点的闸门、入队点的寄放、后台的横幅/卡片/落库。
	// 锁序是硬的：breakerMu → pauseMu → (spillMu | e.mu | siteMu)，不许反向；pauseMu 绝不跨 DB 调用。
	pauseMu      sync.Mutex
	paused       map[pauseScope]pauseState // scope -> 现场（整站条目的 Stage 为空）
	pauseChanged chan struct{}             // 每次变更换一个新的并 close 旧的（叫醒所有等闸门的人）
	lastResumed  map[pauseScope]time.Time  // 放行时刻（信号删了之后后台 payload 还要它）
	wakeDrain    chan struct{}             // 容量 1：恢复时叫醒回灌循环，别让它等下一个 tick

	spillMu        sync.Mutex
	spilled        map[string][]*Task // 阶段键 -> 高水位溢出的待回灌任务
	spilledCount   atomic.Int64       // 累计溢出任务数（监控埋点）
	recoveredCount atomic.Int64       // 累计启动恢复任务数（recover_queue，只在启动跑一次）

	queueRuns     map[string]*queueRunState // 治理队列的运行快照（监控页读取）
	queueCounters map[string]*atomic.Int64  // 队列名 -> 累计重新投递计数

	delayMu   sync.Mutex // 保护 delayHeap
	delayHeap delayHeap  // 延迟投递最小堆
	delayCh   chan struct{}

	// siteStats 各站点的快照（站点信息 + 慢变统计）：启动播种时填，之后每次写库同步更新；
	// 监控页读它（不查库）。库那一份在 crawler_sites 表（见 sitestat.go）。
	siteStats map[string]core.SiteStat
	siteMu    sync.RWMutex

	// errorQueue / repeatQueue 失败重投与周期轮询队列：各**一条全局队列**（不再按站点拆）。
	// 运行态在 NewEngine 里建好，之后只读。
	errorQueue  *errorQueueState
	repeatQueue *repeatQueueState

	recoverQueueMu sync.Mutex // 串行化启动恢复，避免重复投递（ProcessRecoverQueue 是导出的，业务也可能调）
	// tickerWG 动态 ticker 的等待组：Stop 要等它们退出再返回，否则调用方看到 drained=true
	// 就去关库，而在途的那一跳可能正在写库（`sql: database is closed`）。
	tickerWG sync.WaitGroup

	proxy    *proxy.Manager       // 代理管理器中间件
	m3u8     *m3u8.Downloader     // m3u8下载器（全局那份）
	filedown *filedown.Downloader // 文件下载器（全局那份）
	// 站点/阶段覆盖层与解析结果：启动期（ApplyRegisterStage → resolveStageDownloads）建好后**只读**
	//（fetcher 的取用路径不带锁，与 repeatQueues / statsQueue 那几张 map 同一约定）。
	downloadCfgMu   sync.RWMutex
	siteDownloads   map[string]SiteDownloads // 站点层覆盖（声明给）
	m3u8ByStage     map[string]*m3u8.Downloader
	m3u8BySite      map[string]*m3u8.Downloader
	filedownByStage map[string]*filedown.Downloader
	filedownBySite  map[string]*filedown.Downloader
	metrics         *metrics.Registry // 业务自定义监控数据注册表
	notifiers       []Notifier        // 告警通知器，任务最终失败时触发
	// siteBreaker 每站一个**熔断计数器**（滑动窗口数终态失败、判阈），由声明层按站点建
	// （App.RegisterSites 对每个站点调 SetSiteBreaker：单配了阈值的用它的，没单配的用
	// crawler.breaker 那份默认值）。一个站被墙不会把另一个站一起闸住 —— 与阶段名按站点作用配套。
	//
	// 它**不再是闸门**：达到阈值只是"该停了"，停这个动作由 notifyFailure 写进暂停信号、
	// 由取任务点的闸门与入队点的寄放去执行（见 pause.go）。
	breakerMu   sync.Mutex
	siteBreaker map[string]*breaker.Breaker
	// sites 站点声明快照（Key/BaseURL），由 App.RegisterSites 填；handler 用 Site(task.Site) 取。
	// 框架只把它当标签，不做任何限制（见 core.Site 的注释）。
	sites map[string]core.Site
}

// SetSite 登记一份站点声明快照（App.RegisterSites 调）。key 为空时忽略。
func (e *Engine) SetSite(s core.Site) {
	if s.Key == "" {
		return
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if e.sites == nil { // 手工构造的 Engine（测试 / 业务自己拼）没有这一步初始化
		e.sites = make(map[string]core.Site)
	}
	e.sites[s.Key] = s
}

// siteHeaders 取某个站点的请求头（没有返回 nil）。
func (e *Engine) siteHeaders(site string) map[string]string {
	if site == "" {
		return nil
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	return e.sites[site].Headers
}

// Site 取某个站点的声明快照。handler 里典型用法：
//
//	site, _ := engine.Site(task.Site)   // task.Site 随行落库，重投之后照样认得出站点
//	abs := site.BaseURL + rel
func (e *Engine) Site(key string) (core.Site, bool) {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	s, ok := e.sites[key]
	return s, ok
}

// stageInfo 内部阶段信息
type stageInfo struct {
	// stage 阶段名（`GetStage()` 的原值，**不带站点**）。登记表的键是复合键
	// `core.StageKey(config.Site, stage)`，这里留着裸名是给监控快照与日志用的
	//（后台要在站点分组里显示"catalog"这种短名字，而不是 "yeguodj:catalog"）。
	stage      string
	config     StageConfig
	fetcher    Fetcher
	submitFunc func(engine *Engine)
}

// StageConfig 阶段配置
type StageConfig struct {
	MaxAttempts int                  // Handler 最大尝试次数（含首次执行）
	Backoff     time.Duration        // Handler 错误重试退避时间
	Delay       config.DurationRange // 任务间隔延迟，支持随机区间
	WorkerCount int                  // WorkerCount 该阶段专用的 worker 数量
	QueueSize   int                  // QueueSize 该阶段的任务队列缓冲大小

	// Site 本阶段所属站点（`SiteSpec.Key`）。**必填** —— 阶段键是 `site:stage`，熔断按站点
	// 分组、任务表也按它记归属（`crawler_tasks.site`），没有「未归属」那一档。
	Site string

	// Entry 入口模式：`EntryAuto` 启动那一趟就投入口任务（SubmitEntries），
	// `EntryManual` 不自动投、等后台点火（Engine.SubmitStageEntries）。
	// 零值 `EntryOff` 在**声明层**就等于"没有入口"（那边会把回调丢掉），所以引擎侧见不到它：
	// 到了这里还带 off 的，只可能是绕过声明层直接 AddStage 的用法，那一概按"不投"处理。
	Entry EntryMode

	// M3U8 / FileDown 本阶段的下载器**覆盖层**（nil = 不覆盖）：启动时叠在"站点那份 → 全局那份"之上，
	// 解析出这一阶段该用的实例（见 engine/downloads.go）。fetcher 用 `M3U8ForTask(task)` /
	// `FiledownForTask(task)` 取 —— 一行拿到"这一阶段该用的那个"，不会拿错站。
	M3U8     *m3u8.Overlay
	FileDown *filedown.Overlay
}

// AddStage 登记一个阶段。**键由 config.Site + stage 拼出来**（`core.StageKey`）——
// 站点归属取自声明层的 `StageConfig.Site`，所以调用方不可能"忘了带站点"。
// 阶段名只在本站内唯一：两个站登记各自的 `catalog` 是两条独立记录（各自的池子与统计）。
func (e *Engine) AddStage(stage string, config StageConfig, fetcher Fetcher, subFunc func(engine *Engine)) {
	e.stages[core.StageKey(config.Site, stage)] = &stageInfo{
		stage:      stage,
		config:     config,
		fetcher:    fetcher,
		submitFunc: subFunc,
	}
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
		metrics:            metrics.New(),
		delayCh:            make(chan struct{}, 1),
		spilled:            make(map[string][]*Task),
		queueRuns:          newQueueRuns(),
		stageTotals:        make(map[string]stageTotals),
		errorCh:            make(chan error, 100),
		errorForwardCancel: make(map[string]chan struct{}),
		// 容量 1：恢复时叫醒回灌循环（非阻塞发送，重复叫醒只留一次）
		wakeDrain: make(chan struct{}, 1),
	}
	engine.siteBreaker = make(map[string]*breaker.Breaker)
	engine.sites = make(map[string]core.Site)
	// 熔断计数器不在这里建：每站一个、由声明层按站点建（SetSiteBreaker）。
	// 计数走 RecordFailure（终态失败）；达到阈值之后由 notifyFailure 写暂停信号并发一条
	// AlertCritical（业务不用再接一套告警通道）。
	// 两条治理队列（全局各一条）的运行态 + 累计计数：键就是监控面板上的队列名。
	engine.errorQueue = &errorQueueState{}
	engine.repeatQueue = &repeatQueueState{wake: make(chan struct{}, 1)}
	engine.queueCounters = map[string]*atomic.Int64{
		QueueError:  &engine.errorQueue.retried,
		QueueRepeat: &engine.repeatQueue.repolled,
	}
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

// GetDB 获取数据库操作实例
func (e *Engine) GetDB() *gorm.DB {
	return e.db
}

// Upsert 冲突时按 conflictColumns 更新 updateColumns，并回填主键，使 record 指向最终记录。
func (e *Engine) Upsert(record any, conflictColumns, updateColumns []string) error {
	return database.Upsert(e.db, record, conflictColumns, updateColumns)
}

// GetLoggerSet 获取日志管理器列表
func (e *Engine) GetLoggerSet() *loggers.LoggerSet {
	return e.loggerSet
}

// GetConfig 获取全局配置
func (e *Engine) GetConfig() *config.Config {
	return e.cfg
}

// marshalNoHTMLEscape 序列化要写进 JSON 列的值，**关掉 Go 默认的 HTML 转义**。
//
// 默认的 `json.Marshal` 把 `<` `>` `&` 写成 `<` `>` `&`，而业务存进这些列的
// 常常就是一段 HTML（页面片段、快照、富文本）—— 落在库里、后台表格里、SQL 客户端里
// 就是一堆 `<`，"存进去、肉眼扫一眼"这件事直接被劝退。
//
// 转义与否只是 JSON 的两种写法，`json.Unmarshal` 还原出的是同一个字符串 ——
// 所以改这个既不动老数据、也不影响读回（老行里的 `<` 照样读得出来）。
//
// 用 Encoder 而不是 Marshal：只有 Encoder 有 SetEscapeHTML。它会**多写一个换行**，去掉。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SetProxy 设置代理。必须在 ApplyRegisterStage（App.RegisterSites 会走到）之前
// —— 池子/客户端在那一刻就把实例绑好了，之后再设换不回来。
func (e *Engine) SetProxy(proxy *proxy.Manager) {
	e.proxy = proxy
}

// GetProxy 获取代理管理器
func (e *Engine) GetProxy() *proxy.Manager {
	return e.proxy
}

// 下载器**没有全局那一层配置面**（`SetM3U8` / `SetFiledown` 已删除）：
//
//   - 配置只写两处，见 engine/downloads.go —— 站点声明 `SiteSpec.M3U8` / `FileDown`、
//     阶段声明 `StageSpec.M3U8` / `FileDown`（解析顺序：**阶段 > 站点**）；
//   - 基座是框架按 `DefaultConfig()` 备好的那份（`GetM3U8` / `GetFiledown` 返回的就是它）；
//   - fetcher 里取"这一阶段该用的那个"用 `M3U8ForTask(task)` / `FiledownForTask(task)`；
//     要临时一份只给这次调用用，`.With(...)`；要完全独立的一份，`NewDownloader(&cfg)`。

// GetM3U8 获取 m3u8 下载器：**框架默认那份**（基座）。
// 按站点/阶段配过的那一份要用 `M3U8ForTask(task)` / `M3U8ForStage(stage)` 取。
func (e *Engine) GetM3U8() *m3u8.Downloader {
	return e.m3u8
}

// GetFiledown 获取文件下载器：**框架默认那份**（基座）。
// 按站点/阶段配过的那一份要用 `FiledownForTask(task)` / `FiledownForStage(stage)` 取。
func (e *Engine) GetFiledown() *filedown.Downloader {
	return e.filedown
}

// SetSiteBreaker 为一个站点建一把熔断闸门。
//
// **声明层会给每个声明的站点都调它**（App.RegisterSites 按 SiteSpec 逐个来）：单配了阈值的用它的，
// 没单配的用 crawler.breaker 那份默认 —— 关键在于"每站一把"，而不是"没声明的站共用默认那把"
// （共用的话 A 站被墙会把 B 站一起闸住）。
//
// 必须在 ApplyRegisterStage 之前调用（池子在那一刻就把闸门绑好了，之后再设换不回来）。
// site 为空是非法参数：站点是必填维度，没有"未归属"那一档。
func (e *Engine) SetSiteBreaker(site string, cfg breaker.Config) {
	if site == "" {
		return
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if e.siteBreaker == nil {
		e.siteBreaker = make(map[string]*breaker.Breaker)
	}
	// 这里只登记**计数配置**：熔断器不再持有暂停状态、也不发告警 ——
	// 达到阈值之后由 notifyFailure 写一条暂停信号（Source=breaker）并发 AlertCritical。
	e.siteBreaker[site] = breaker.New(cfg)
}

// breakerFor 取某个站点的熔断计数器；没这个站（或没给它 SetSiteBreaker）返回 nil。
//
// 返回 nil 是常态的一种（自己搭引擎没配熔断、旧库里 site 为空的行）：`*breaker.Breaker`
// 的所有方法都容忍 nil 接收者，所以调用处不必判空 —— 没配就是"没有熔断这回事"。
//
// 注意它**不再是闸门**：暂停与它无关（那是 pause.go 里那条信号）。
func (e *Engine) breakerFor(site string) *breaker.Breaker {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	return e.siteBreaker[site]
}

// ResumeCrawling 放行**所有站点**的暂停，返回是否真的放行了至少一个。
// 这就是后台横幅上那个「恢复抓取」；只想放行某一站用 ResumeSite。
//
// 它清的是**暂停信号**（见 pause.go）—— 熔断触发与人工暂停写的是同一张表，所以这里一视同仁地放行。
func (e *Engine) ResumeCrawling() bool {
	if !e.clearAllPauses() {
		return false
	}
	// 放行之后立刻把寄放在溢出表里的任务灌回去（不等下一个 drain tick）
	e.wakeDrainNow()
	return true
}

// ResumeSite 放行某个站点（该站所有暂停条目一起清，见 clearSite 的理由）；
// 没这个站、或它本来就没暂停返回 false。
func (e *Engine) ResumeSite(site string) bool {
	if !e.knownSite(site) {
		return false
	}
	if !e.clearSite(site) {
		return false
	}
	e.wakeDrainNow()
	// 落库放在放锁之后：pauseMu 绝不跨 DB 调用（锁序见 pause.go）。
	// 熔断**触发**不写这一列（与今天一致）：它只写暂停信号，横幅读信号、站点表读库，两者在触发时
	// 本来就不同步 —— 这是有意的，别顺手把两条路并成一条。
	e.writeSiteBreakerState(site, false)
	return true
}

// PauseSite 手动暂停某个站点的抓取；不认识的站点返回 false（后台据此回 400）。
//
// 现场里带上当时的窗口失败数（与今天一致：它是后台 payload 的一个字段，别顺手改成 0）。
func (e *Engine) PauseSite(site, reason string) bool {
	if !e.knownSite(site) {
		return false
	}
	changed := e.setPause(pauseScope{Site: site}, pauseState{
		Reason: reason, Source: PauseSourceManual,
		Failures: e.breakerSum(site),
	})
	if !changed {
		// 已经停着（可能正是熔断触发的那一条）：返回 false，也**不覆盖**它的 reason 与快照 ——
		// 后台"空点不记审计"那条规则、以及"人工暂停不该把熔断现场改写成手动"都靠这个。
		return false
	}
	e.writeSiteBreakerState(site, true)
	return true
}

// PauseCrawling 手动闸住抓取（后台/业务都可用）：**所有配了抓取策略的站点**一起暂停，
// 返回是否真的至少停了一个。
func (e *Engine) PauseCrawling(reason string) bool {
	sites := e.allSites()
	sort.Strings(sites)
	paused := false
	for _, site := range sites {
		if e.setPause(pauseScope{Site: site}, pauseState{
			Reason: reason, Source: PauseSourceManual,
			Failures: e.breakerSum(site),
		}) {
			paused = true
		}
	}
	// 与 PauseSite 一样**不写 crawler_sites**（今天的 PauseCrawling 也不写）。
	return paused
}

// breakerSum 该站当前窗口内的终态失败数（没配熔断/没这个站都是 0）。
func (e *Engine) breakerSum(site string) int {
	if b := e.breakerFor(site); b != nil {
		return b.Sum()
	}
	return 0
}

// BreakerStatuses 返回各站点的熔断状态快照：key 是站点 Key（站点是必填维度，没有空键那一档）。
//
// 列出**有闸门**的站点 —— 每个声明的站点各一条（不管有没有单配阈值）。**每一把都要列**，
// 包括没启用、也没暂停的：后台的「恢复抓取」端点 targets 取自这张表，不列就等于
// "那个站闸住了却既看不见、也点不到恢复"，只能重启或让业务代码自己调 ResumeCrawling。
func (e *Engine) BreakerStatuses() map[string]BreakerStatus {
	sites := e.allSites()
	sort.Strings(sites)
	out := make(map[string]BreakerStatus, len(sites))
	for _, site := range sites {
		out[site] = e.breakerStatusOf(site)
	}
	return out
}

// breakerStatusOf 一个站点的快照：**合成视图** —— 统计与配置取自熔断器，暂停那几列取自暂停信号。
//
// 为什么合成：暂停状态现在只有一处（pause.go），而后台的历史契约是"一条记录里既有熔断阈值
// 又有暂停现场"。合成让 payload 的字段形状一个字都不变（只多一个 Source）。
func (e *Engine) breakerStatusOf(site string) BreakerStatus {
	var st BreakerStatus
	if b := e.breakerFor(site); b != nil {
		st.Enabled = b.Enabled()
		st.Window = b.Window()
		st.Threshold = b.Threshold()
		st.InWindow = b.Sum()
	}
	st.Site = site
	// 用"该站任一暂停条目"而不是只查整站那条：阶段级暂停也该在这一行里显示出来
	//（这样才能一眼看出"这一站停的是哪个阶段"）。
	info := e.pauseInfoOf(site)
	if info.Paused {
		st.Paused = true
		st.PausedAt = info.State.At
		st.Reason = info.State.Reason
		st.Source = info.State.Source
		// stage 先说**scope**（阶段级暂停停的是它），scope 是整站时才退回触发现场那个阶段：
		// 熔断触发一定带"当时哪个阶段在失败"，手动整站暂停两者都为空。
		st.Stage = info.Scope.Stage
		if st.Stage == "" {
			st.Stage = info.State.Stage
		}
		st.Failures = info.State.Failures
	}
	st.ResumedAt = info.ResumedAt
	return st
}

// Stop 停止引擎：先取消引擎 ctx（让在途的退避等待尽快结束），再让各阶段工作池排空队列。
//
// 返回 drained 表示是否**所有**阶段与后台动态 ticker（错误/恢复/每站的轮询队列）都在 timeout
// 内停下；没排空时 stats 是那一刻的存留情况（已排空的阶段不会出现在 map 里）。ticker 也算在内，
// / 是因为它们同样会写库 —— 不等它们就关库，在途的那一跳会报 `sql: database is closed`。
//
// 各阶段的 timeout 是**并发**计的，所以总等待约等于一个 timeout，而不是「阶段数 × timeout」。
//
// 拿到 drained=false 时，调用方**不该接着关数据库 / 关浏览器池**：worker goroutine 还活着
// （Go 杀不掉它，见 workerpool.Stop），关掉只会让在途的写入全部报 "sql: database is closed"。
func (e *Engine) Stop(timeout time.Duration) (drained bool, stats map[string]StopStats) {
	e.cancel()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	stats = make(map[string]StopStats)
	// 遍历活池表（而不是 e.stages）：池子是懒建/回收的，e.stages 里「没建池」的阶段
	// 不在表里，天然被跳过 —— 关停这一步不该自己 panic 掉。
	for name, pool := range e.pools.snapshot() {
		wg.Add(1)
		go func(stage string, pool *workerpool.WorkerPool[*Task]) {
			defer wg.Done()
			d, inFlight := pool.Stop(timeout)
			if d {
				return
			}
			main, urgent := pool.QueueDepths()
			mu.Lock()
			stats[stage] = StopStats{Unfinished: inFlight, Queued: main + urgent}
			mu.Unlock()
		}(name, pool)
	}
	wg.Wait()
	// 溢出表里**寄放**的任务也算进存留：被暂停的站点，任务不在任何池子的队列里（那正是收口的语义），
	// 只报池子的存留就会得出 drained=true —— 调用方（app.Run）随即去关库，而几十上百条任务
	// 还在溢出表里躺着。它们的行是 pending，重启后由启动恢复捞，但那一刻**运维看不到任何告警**。
	// 这与池子队列里的任务同一性质（"这一轮没跑完"），所以并进同一个结构。
	e.spillMu.Lock()
	for key, tasks := range e.spilled {
		if len(tasks) == 0 {
			continue
		}
		st := stats[key]
		st.Queued += len(tasks)
		stats[key] = st
	}
	e.spillMu.Unlock()
	drained = len(stats) == 0
	// 动态 ticker（错误/恢复/每站的轮询队列）也要等在途的那一跳跑完：它们同样会写库。
	// 与阶段池共用同一个 timeout（并发计），等不到就如实说"没排空"。
	tickersDone := make(chan struct{})
	go func() {
		e.tickerWG.Wait()
		close(tickersDone)
	}()
	select {
	case <-tickersDone:
	case <-time.After(timeout):
		drained = false
		e.loggerSet.Engine.Warn("dynamic tickers still running after timeout")
	}
	if drained {
		e.loggerSet.Engine.Info("all workers stopped")
	}
	return drained, stats
}

// Errors 返回引擎的常驻错误通道（单条）：所有池的错误都由各自的转发协程汇到这一条上，
// mdMsgListener 只读它。池是懒建的，逐个取池错误通道会漏掉「启动之后才建的池」——
// 所以这里是常驻一条，而不是启动时快照。
func (e *Engine) Errors() (errors []<-chan error) {
	if e.errorCh == nil {
		return nil
	}
	return []<-chan error{e.errorCh}
}
