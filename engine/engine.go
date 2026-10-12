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
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"github.com/ydtg1993/papa/v3/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
	"gorm.io/datatypes"
	"gorm.io/gorm"
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
	stages    map[string]*stageInfo
	// pools 活池登记表：stage 名 -> 当前活着的池。投递/关停/错误通道都从它取池，
	// 不再散落在 stageInfo.workerPool 上（那是裸指针，池一旦懒建/回收就会并发读写）。
	pools              *poolRegistry
	mu                 sync.RWMutex
	cfg                *config.Config
	browserPool        *browser.Pool
	htmlClient         *htmlfetch.Client
	statsQueue         map[string]*track.StatsQueue[*Task] // key: stage name 分阶段监控信号
	stageTotals        map[string]stageTotals              // 各阶段累计吞吐（池 churn 不清零）；e.mu 保护
	errorCh            chan error                          // 常驻错误通道：所有池的错误兜底转发到这里，mdMsgListener 读它
	errorForwardCancel map[string]chan struct{}            // per-stage 错误转发协程停机信号（reap 时 close）；e.mu 保护
	dedupCache         *dedupCache                         // 有界去重表（LRU），key: 任务去重键；淘汰条目由 DB 唯一索引兜底

	spillMu        sync.Mutex
	spilled        map[string][]*Task // stage -> 高水位溢出的待回灌任务
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
	// 熔断闸门。**按站点分组**：一台闸门管一个站点的（该站所有阶段的）worker；
	// 默认 scope（未归属站点的阶段）就是 breaker 这一把 —— 单站项目的行为与之前完全一致。
	// 多站时站点 A 被墙不会再把站点 B 一起闸住（见 SetSiteBreaker / breakerFor）。
	breaker     *breaker.Breaker
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

	// Site 本阶段所属站点（`SiteSpec.Key`）。**空 = 未归属**，落在"默认 scope"上 ——
	// 熔断用它分组、任务表用它记归属（`crawler_tasks.site`）、监控与日志也按它分维度。
	Site string

	// M3U8 / FileDown 本阶段的下载器**覆盖层**（nil = 不覆盖）：启动时叠在"站点那份 → 全局那份"之上，
	// 解析出这一阶段该用的实例（见 engine/downloads.go）。fetcher 用 `M3U8ForTask(task)` /
	// `FiledownForTask(task)` 取 —— 一行拿到"这一阶段该用的那个"，不会拿错站。
	M3U8     *m3u8.Overlay
	FileDown *filedown.Overlay
}

func (e *Engine) AddStage(stage string, config StageConfig, fetcher Fetcher, subFunc func(engine *Engine)) {
	e.stages[stage] = &stageInfo{
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
	}
	engine.siteBreaker = make(map[string]*breaker.Breaker)
	engine.sites = make(map[string]core.Site)
	// 熔断器：计数走 RecordFailure（终态失败），触发时回调发一条 AlertCritical。
	// 把 engine 自己传进去是为了复用同一套 Notifier —— 业务不用再接一套告警通道。
	engine.breaker = breaker.New(breaker.Config{
		Enabled:   cfg.Crawler.Breaker.Enabled,
		Window:    cfg.Crawler.Breaker.WindowOrDefault(),
		Threshold: cfg.Crawler.Breaker.Threshold,
	}, engine.notifyBreakerTrip)
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

// SaveResult 写任务结果：title 写入 title 列，content 序列化为 JSON 写入 content 列。
func (e *Engine) SaveResult(taskID int, title string, content any) error {
	b, err := marshalNoHTMLEscape(content)
	if err != nil {
		return err
	}
	return e.db.Model(&models.CrawlerTask{}).
		Where("id = ?", taskID).
		Updates(map[string]any{
			"title":   title,
			"content": datatypes.JSON(b),
		}).Error
}

// SaveContent 仅更新 content 列（保留 title），用于回写已有任务的结果。
func (e *Engine) SaveContent(taskID int, content any) error {
	b, err := marshalNoHTMLEscape(content)
	if err != nil {
		return err
	}
	return e.db.Model(&models.CrawlerTask{}).
		Where("id = ?", taskID).
		Update("content", datatypes.JSON(b)).Error
}

// GetResult 读回任务的 content 列并反序列化到 out（out 需为指针）。
func (e *Engine) GetResult(taskID int, out any) error {
	var rec models.CrawlerTask
	if err := e.db.Where("id = ?", taskID).First(&rec).Error; err != nil {
		return err
	}
	if len(rec.Content) == 0 {
		return nil
	}
	return json.Unmarshal(rec.Content, out)
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

// SetSiteBreaker 为一个站点单独配一把熔断闸门（覆盖 crawler.breaker 那份默认值）。
//
// 必须在 ApplyRegisterStage 之前调用（池子在那一刻就把闸门绑好了，之后再设换不回来）——
// App.RegisterSites 会按 SiteSpec 逐个调它。site 为空是非法参数（那正是"默认 scope"，
// 它用 crawler.breaker 那把，不需要也不允许单独配）。
func (e *Engine) SetSiteBreaker(site string, cfg breaker.Config) {
	if site == "" {
		return
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if e.siteBreaker == nil {
		e.siteBreaker = make(map[string]*breaker.Breaker)
	}
	// 回调里补上站点：告警文案与后台都要能看出"是哪个站被闸住了"（只带阶段名不够）
	e.siteBreaker[site] = breaker.New(cfg, func(st BreakerStatus) {
		st.Site = site
		e.notifyBreakerTrip(st)
	})
}

// breakerFor 取某个站点（scope）的熔断闸门；site 为空或没单独配过 → 用默认那把。
func (e *Engine) breakerFor(site string) *breaker.Breaker {
	if site == "" {
		return e.breaker
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	if b, ok := e.siteBreaker[site]; ok {
		return b
	}
	return e.breaker
}

// ResumeCrawling 放行**所有** scope 的熔断闸门（默认 scope + 各站点），返回是否真的放行了至少一个。
// 单站项目与以前完全一致；多站时这就是后台横幅上那个「恢复抓取」。
func (e *Engine) ResumeCrawling() bool {
	resumed := e.breaker.Resume()
	e.breakerMu.Lock()
	sites := make([]*breaker.Breaker, 0, len(e.siteBreaker))
	for _, b := range e.siteBreaker {
		sites = append(sites, b)
	}
	e.breakerMu.Unlock()
	for _, b := range sites {
		if b.Resume() {
			resumed = true
		}
	}
	return resumed
}

// ResumeSite 放行某个站点（scope）的闸门；site 为空 = 默认 scope。
// 没有这把闸门（该站没单独配过、也没有默认 scope）时返回 false。
func (e *Engine) ResumeSite(site string) bool {
	b := e.siteBreakerOf(site)
	if b == nil || !b.Resume() {
		return false
	}
	e.writeSiteBreakerState(site, false)
	return true
}

// PauseSite 手动暂停某个站点（scope）的闸门；site 为空 = 默认 scope。
// 没有这把闸门时返回 false（后台据此回 400：那个 scope 不存在）。
func (e *Engine) PauseSite(site, reason string) bool {
	b := e.siteBreakerOf(site)
	if b == nil || !b.Pause(reason) {
		return false
	}
	e.writeSiteBreakerState(site, true)
	return true
}

// siteBreakerOf 取某个 scope 的闸门对象；不存在的返回 nil（与 breakerFor 的"回落默认"不同 ——
// 手动干预要能区分"这个 scope 不存在"，静默作用到默认那把会很意外）。
func (e *Engine) siteBreakerOf(site string) *breaker.Breaker {
	if site == "" {
		return e.breaker
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	return e.siteBreaker[site]
}

// PauseCrawling 手动闸住抓取（后台/业务都可用）：**所有** scope 一起闸，返回是否真的切到暂停态。
func (e *Engine) PauseCrawling(reason string) bool {
	paused := e.breaker.Pause(reason)
	e.breakerMu.Lock()
	sites := make([]*breaker.Breaker, 0, len(e.siteBreaker))
	for _, b := range e.siteBreaker {
		sites = append(sites, b)
	}
	e.breakerMu.Unlock()
	for _, b := range sites {
		if b.Pause(reason) {
			paused = true
		}
	}
	return paused
}

// BreakerStatus 返回**默认 scope**（未归属站点的阶段）的熔断状态快照。
// 多站时要看全部，用 BreakerStatuses。
func (e *Engine) BreakerStatus() BreakerStatus { return e.breaker.Status() }

// BreakerStatuses 返回各 scope（站点）的熔断状态快照：key 是站点 Key，"" 是默认 scope。
//
// 只列出**真的有闸门**的 scope：单独配过的站点，加上默认那把（如果启用了熔断）。
func (e *Engine) BreakerStatuses() map[string]BreakerStatus {
	out := make(map[string]BreakerStatus)
	// 默认 scope：**启用**时列出来（它自动管着未归属站点的阶段）；没启用但**被手动暂停过**时
	// 也得列出来 —— 后台的「恢复抓取」端点 targets 取自这张表，不列就等于"闸住了却既看不见、
	// 也点不到恢复"，只能重启或让业务代码自己调 ResumeCrawling。
	if st := e.breaker.Status(); e.breaker.Enabled() || st.Paused {
		st.Site = "" // 留空 = 默认 scope
		out[""] = st
	}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	for site, b := range e.siteBreaker {
		st := b.Status()
		st.Site = site
		out[site] = st
	}
	return out
}

// siteOf 取某个阶段所属的站点 Key（未注册或未归属返回空串 = 默认 scope）。
func (e *Engine) siteOf(stage string) string {
	if info := e.stages[stage]; info != nil {
		return info.config.Site
	}
	return ""
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
