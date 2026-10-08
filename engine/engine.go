package engine

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/internal/breaker"
	"github.com/ydtg1993/papa/v2/internal/database"
	"github.com/ydtg1993/papa/v2/internal/metrics"
	"github.com/ydtg1993/papa/v2/internal/track"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
	"github.com/ydtg1993/papa/v2/pkg/browser"
	"github.com/ydtg1993/papa/v2/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
	"github.com/ydtg1993/papa/v2/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v2/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
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
	breaker   *breaker.Breaker     // 熔断闸门：窗口内终态失败超阈值就闸住所有阶段的 worker
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

// SaveResult 写任务结果：title 写入 title 列，content 序列化为 JSON 写入 content 列。
func (e *Engine) SaveResult(taskID int, title string, content any) error {
	b, err := json.Marshal(content)
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
	b, err := json.Marshal(content)
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

// SetProxy 设置代理 需要在RegisterStage之前设置
func (e *Engine) SetProxy(proxy *proxy.Manager) {
	e.proxy = proxy
}

// GetProxy 获取代理管理器
func (e *Engine) GetProxy() *proxy.Manager {
	return e.proxy
}

// SetM3U8 设置m3u8下载器 需要在RegisterStage之前设置
func (e *Engine) SetM3U8(m3u *m3u8.Downloader) {
	e.m3u8 = m3u
}

// GetM3U8 获取m3u8下载器
func (e *Engine) GetM3U8() *m3u8.Downloader {
	return e.m3u8
}

// SetFiledown 设置文件下载器 需要在RegisterStage之前设置
func (e *Engine) SetFiledown(f *filedown.Downloader) {
	e.filedown = f
}

// GetFiledown 获取文件下载器实例
func (e *Engine) GetFiledown() *filedown.Downloader {
	return e.filedown
}

// ResumeCrawling 放行被熔断闸住的抓取。本来就没闸住返回 false。
func (e *Engine) ResumeCrawling() bool { return e.breaker.Resume() }

// PauseCrawling 手动闸住抓取（后台/业务都可用）。已在暂停态返回 false。
func (e *Engine) PauseCrawling(reason string) bool { return e.breaker.Pause(reason) }

// BreakerStatus 返回熔断闸门的当前状态快照。
func (e *Engine) BreakerStatus() BreakerStatus { return e.breaker.Status() }

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
