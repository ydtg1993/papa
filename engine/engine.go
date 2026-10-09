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

	ctx         context.Context
	cancel      context.CancelFunc
	stages      map[string]*stageInfo
	mu          sync.RWMutex
	cfg         *config.Config
	runtime     atomic.Pointer[config.RuntimeConfig] // 运行期动态配置覆盖层（delta）
	browserPool *browser.Pool
	htmlClient  *htmlfetch.Client
	statsQueue  map[string]*track.StatsQueue[*Task] // key: stage name 分阶段监控信号
	dedupCache  *dedupCache                         // 有界去重表（LRU），key: 任务去重键；淘汰条目由 DB 唯一索引兜底

	spillMu             sync.Mutex
	spilled             map[string][]*Task // stage -> 高水位溢出的待回灌任务
	spilledCount        atomic.Int64       // 累计溢出任务数（监控埋点）
	recoveredCount      atomic.Int64       // 累计启动恢复任务数（recover_queue，只在启动跑一次）
	errorRetriedCount   atomic.Int64       // 累计失败重投任务数（error_queue）
	repeatRepolledCount atomic.Int64       // 累计周期轮询重投任务数（repeat_queue）

	queueRuns     map[string]*queueRunState // 治理队列的运行快照（监控页读取）
	queueCounters map[string]*atomic.Int64  // 队列名 -> 累计重新投递计数

	delayMu   sync.Mutex // 保护 delayHeap
	delayHeap delayHeap  // 延迟投递最小堆
	delayCh   chan struct{}

	configChanged chan struct{} // 运行期配置变更信号（唤醒动态 ticker 重新读生效配置）

	errorQueueMu   sync.Mutex // 串行化错误队列处理，避免自动+手动并发重复投递
	recoverQueueMu sync.Mutex // 串行化启动恢复，避免重复投递（ProcessRecoverQueue 是导出的，业务也可能调）
	repeatQueueMu  sync.Mutex // 串行化周期轮询队列处理，避免自动+手动并发重复投递

	proxy     *proxy.Manager       // 代理管理器中间件
	m3u8      *m3u8.Downloader     // m3u8下载器
	filedown  *filedown.Downloader // 文件下载器
	metrics   *metrics.Registry    // 业务自定义监控数据注册表
	notifiers []Notifier           // 告警通知器，任务最终失败时触发
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
	workerPool *workerpool.WorkerPool[*Task]
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
		ctx:           ctx,
		cancel:        cancel,
		stages:        make(map[string]*stageInfo),
		dedupCache:    newDedupCache(cfg.Crawler.DedupCacheSize),
		db:            db,
		cfg:           cfg,
		loggerSet:     loggerSet,
		metrics:       metrics.New(),
		delayCh:       make(chan struct{}, 1),
		configChanged: make(chan struct{}, 1),
		spilled:       make(map[string][]*Task),
		queueRuns:     newQueueRuns(),
	}
	engine.runtime.Store(&config.RuntimeConfig{})
	engine.siteBreaker = make(map[string]*breaker.Breaker)
	engine.sites = make(map[string]core.Site)
	// 熔断器：计数走 RecordFailure（终态失败），触发时回调发一条 AlertCritical。
	// 把 engine 自己传进去是为了复用同一套 Notifier —— 业务不用再接一套告警通道。
	engine.breaker = breaker.New(breaker.Config{
		Enabled:   cfg.Crawler.Breaker.Enabled,
		Window:    cfg.Crawler.Breaker.WindowOrDefault(),
		Threshold: cfg.Crawler.Breaker.Threshold,
	}, engine.notifyBreakerTrip)
	engine.queueCounters = map[string]*atomic.Int64{
		QueueError:  &engine.errorRetriedCount,
		QueueRepeat: &engine.repeatRepolledCount,
	}
	engine.loadActiveTasks()
	go engine.delayDispatcher()
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

// ApplyRuntimeConfig 应用运行期动态配置：只 tunable 字段原地热更（池大小需重启）。
//
// 传进来的是一份**增量**，按字段合并进当前覆盖层（见 config.RuntimeConfig.Merge）：
// 没提到的字段保持原样。整体替换的话，只提交 html.timeout 就会把之前设的
// browser.headers、各队列的 interval 悄无声息地清掉。
func (e *Engine) ApplyRuntimeConfig(rt *config.RuntimeConfig) error {
	// 先校验再合并：**这条路绕过 config.Load**（LoadRuntime 只 yaml.Unmarshal、
	// PUT /api/config 只 Merge + Store），不在这里拦就没人拦了 ——
	// 一个 max_body_size: 0 能让每次抓取都失败，而配置文件那边的校验看不见它。
	if err := config.ValidateRuntime(rt); err != nil {
		return err
	}
	merged := e.runtime.Load().Merge(rt)
	e.runtime.Store(merged)

	if e.browserPool != nil {
		e.browserPool.SetHeaders(e.browserHeaders())
		e.browserPool.SetMaxIdleTime(e.browserMaxIdle())
	}

	if e.htmlClient != nil {
		e.htmlClient.SetConfig(e.htmlConfig())
	}
	// 通知动态 ticker 重新读取生效配置（队列 enabled/interval 等）
	select {
	case e.configChanged <- struct{}{}:
	default:
	}
	return nil
}

// GetRuntimeConfig 返回当前运行期覆盖层（只读，调用方勿修改）。
func (e *Engine) GetRuntimeConfig() *config.RuntimeConfig {
	return e.runtime.Load()
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

// SetM3U8 设置 m3u8 下载器。必须在 ApplyRegisterStage 之前（见 SetProxy 的说明）。
func (e *Engine) SetM3U8(m3u *m3u8.Downloader) {
	e.m3u8 = m3u
}

// GetM3U8 获取m3u8下载器
func (e *Engine) GetM3U8() *m3u8.Downloader {
	return e.m3u8
}

// SetFiledown 设置文件下载器。必须在 ApplyRegisterStage 之前（见 SetProxy 的说明）。
func (e *Engine) SetFiledown(f *filedown.Downloader) {
	e.filedown = f
}

// GetFiledown 获取文件下载器实例
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
	return b != nil && b.Resume()
}

// PauseSite 手动暂停某个站点（scope）的闸门；site 为空 = 默认 scope。
// 没有这把闸门时返回 false（后台据此回 400：那个 scope 不存在）。
func (e *Engine) PauseSite(site, reason string) bool {
	b := e.siteBreakerOf(site)
	return b != nil && b.Pause(reason)
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
	if e.breaker.Enabled() {
		out[""] = e.breaker.Status() // Site 留空 = 默认 scope
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
// 返回 drained 表示是否**所有**阶段都在 timeout 内排空；没排空时 stats 是那一刻的存留情况
// （已排空的阶段不会出现在 map 里）。
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
	for name, s := range e.stages {
		// 阶段注册了但没走到 ApplyRegisterStage（池子还没建）时跳过 ——
		// 关停这一步不该自己 panic 掉。
		if s.workerPool == nil {
			continue
		}
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
		}(name, s.workerPool)
	}
	wg.Wait()
	drained = len(stats) == 0
	if drained {
		e.loggerSet.Engine.Info("all workers stopped")
	}
	return drained, stats
}

// Errors 返回work pool中的错误消息队列
func (e *Engine) Errors() (errors []<-chan error) {
	for _, s := range e.stages {
		errors = append(errors, s.workerPool.Errors())
	}
	return
}
