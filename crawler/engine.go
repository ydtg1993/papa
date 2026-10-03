package crawler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/config"
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
	"maps"
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
	recoveredCount      atomic.Int64       // 累计恢复任务数（recover_queue）
	errorRetriedCount   atomic.Int64       // 累计失败重投任务数（error_queue）
	repeatRepolledCount atomic.Int64       // 累计周期轮询重投任务数（repeat_queue）

	queueRuns     map[string]*queueRunState // 三个治理队列的运行快照（监控页读取）
	queueCounters map[string]*atomic.Int64  // 队列名 -> 累计重新投递计数

	delayMu   sync.Mutex // 保护 delayHeap
	delayHeap delayHeap  // 延迟投递最小堆
	delayCh   chan struct{}

	configChanged chan struct{} // 运行期配置变更信号（唤醒动态 ticker 重新读生效配置）

	errorQueueMu   sync.Mutex // 串行化错误队列处理，避免自动+手动并发重复投递
	recoverQueueMu sync.Mutex // 串行化中断恢复队列处理，避免自动+手动并发重复投递
	repeatQueueMu  sync.Mutex // 串行化周期轮询队列处理，避免自动+手动并发重复投递

	proxy     *proxy.Manager       // 代理管理器中间件
	m3u8      *m3u8.Downloader     // m3u8下载器
	filedown  *filedown.Downloader // 文件下载器
	metrics   *metrics.Registry    // 业务自定义监控数据注册表
	notifiers []Notifier           // 告警通知器，任务最终失败时触发
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

// StageStats 阶段统计快照（纯值类型，供监控页读取，不暴露内部实现）
type StageStats struct {
	Global  GlobalStats
	Workers map[int]WorkerStat
	Queue   QueueStats
}

// GlobalStats 阶段全局统计
type GlobalStats struct {
	TotalTasks  int64
	TotalFailed int64
	TotalTime   time.Duration
	AvgTime     time.Duration
	MaxTime     time.Duration
	MinTime     time.Duration
}

// WorkerStat 单个 worker 统计
type WorkerStat struct {
	WorkerID    int
	TotalTasks  int64
	FailedTasks int64
	TotalTime   time.Duration
	MaxTime     time.Duration
	MinTime     time.Duration
}

// QueueStats 阶段队列计数
type QueueStats struct {
	Submitted  int64
	Completed  int64
	Failed     int64
	InProgress int64
	QueueLen   int
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
	engine.queueCounters = map[string]*atomic.Int64{
		QueueError:   &engine.errorRetriedCount,
		QueueRecover: &engine.recoveredCount,
		QueueRepeat:  &engine.repeatRepolledCount,
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

// AddNotifier 注册告警通知器；任务最终失败时触发 Notify。
func (e *Engine) AddNotifier(n Notifier) {
	if n == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifiers = append(e.notifiers, n)
}

func (e *Engine) getNotifiers() []Notifier {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Notifier, len(e.notifiers))
	copy(out, e.notifiers)
	return out
}

// notifyFailure 结构化记录失败日志，并触发告警通知器。
func (e *Engine) notifyFailure(ctx context.Context, task *Task, err error) {
	te := TaskError{
		Stage:   task.Stage,
		TaskID:  task.ID,
		URL:     task.URL,
		Retry:   task.Retry,
		Kind:    ErrorKind(err),
		Message: err.Error(),
	}
	e.loggerSet.Engine.WithFields(logrus.Fields{
		"stage":   te.Stage,
		"task_id": te.TaskID,
		"url":     te.URL,
		"retry":   te.Retry,
		"kind":    te.Kind,
	}).Errorf("task failed: %s", te.Message)

	notifiers := e.getNotifiers()
	if len(notifiers) == 0 {
		return
	}
	level := AlertError
	if Retryable(err) {
		level = AlertWarn
	}
	event := AlertEvent{Level: level, TaskError: te}
	for _, n := range notifiers {
		if nerr := n.Notify(ctx, event); nerr != nil {
			e.loggerSet.Engine.Errorf("notify alert failed: %s", nerr.Error())
		}
	}
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

// RecordMetric 写入一条业务自定义监控数据，供监控页展示
func (e *Engine) RecordMetric(key string, v any) {
	if e.metrics == nil {
		return
	}
	e.metrics.Set(key, v)
}

// GetMetrics 返回监控数据快照：业务自定义数据 + 框架级队列治理计数（溢出/恢复/失败重投）。
func (e *Engine) GetMetrics() map[string]any {
	base := map[string]any{}
	if e.metrics != nil {
		base = e.metrics.GetAll()
	}
	base["queue_spilled"] = e.spilledCount.Load()
	base["queue_spill_backlog"] = e.spillBacklog()
	base["recover_total"] = e.recoveredCount.Load()
	base["error_retry_total"] = e.errorRetriedCount.Load()
	base["repeat_repoll_total"] = e.repeatRepolledCount.Load()
	return base
}

// spillBacklog 返回当前溢出列表中待回灌的任务总数。
func (e *Engine) spillBacklog() int {
	e.spillMu.Lock()
	defer e.spillMu.Unlock()
	n := 0
	for _, tasks := range e.spilled {
		n += len(tasks)
	}
	return n
}

// defaultHeaders 浏览器与静态 HTML 客户端共用的默认请求头，配置中的 headers 会覆盖同名项
var defaultHeaders = map[string]string{
	"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
	"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
}

// SetBrowserPool 创建浏览器操作池，未启用时跳过
func (e *Engine) SetBrowserPool() {
	if !e.cfg.Browser.Enable {
		return
	}
	pool, err := browser.NewPool(browser.PoolConfig{
		Size:           e.cfg.Browser.PoolSize,
		DirectSize:     e.cfg.Browser.DirectSize,
		MaxIdleTime:    e.browserMaxIdle(),
		Headless:       e.cfg.Browser.Headless,
		NoSandbox:      e.cfg.Browser.NoSandbox,
		Leakless:       e.cfg.Browser.Leakless,
		BrowserPath:    e.cfg.Browser.BrowserPath,
		Flags:          map[string]string{},
		DefaultHeaders: e.browserHeaders(),
		ProxyManager:   e.GetProxy(),
	})
	if err != nil {
		panic(fmt.Errorf("new browser pool: %s", err.Error()))
	}
	e.browserPool = pool
}

// GetBrowserPool 获取浏览器池
func (e *Engine) GetBrowserPool() *browser.Pool {
	return e.browserPool
}

// GetHTMLClient 获取静态 HTML 抓取客户端
func (e *Engine) GetHTMLClient() *htmlfetch.Client {
	return e.htmlClient
}

// FetchHTML 抓取并解析静态 HTML 页面，不创建浏览器实例
func (e *Engine) FetchHTML(ctx context.Context, rawURL string) (*htmlfetch.Page, error) {
	return e.htmlClient.Fetch(ctx, rawURL)
}

// SetHTMLClient 创建静态 HTML 抓取客户端，未启用时跳过
func (e *Engine) SetHTMLClient() {
	if !e.cfg.HTML.Enable {
		return
	}
	e.htmlClient = htmlfetch.NewClient(e.htmlConfig())
}

// browserHeaders 合并内置默认头、基础配置、运行期覆盖，返回生效的浏览器默认请求头（每次新建 map）。
func (e *Engine) browserHeaders() map[string]string {
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.Browser.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.Browser.Headers)
	maps.Copy(headers, e.runtime.Load().Browser.Headers)
	return headers
}

// browserMaxIdle 返回生效的浏览器空闲回收阈值。
func (e *Engine) browserMaxIdle() time.Duration {
	rt := e.runtime.Load()
	if rt.Browser.MaxIdleTime != nil {
		return rt.Browser.MaxIdleTime.Duration
	}
	return e.cfg.Browser.MaxIdleTime
}

// htmlConfig 计算生效的静态 HTML 客户端配置（基础 + 运行期覆盖）。
func (e *Engine) htmlConfig() htmlfetch.Config {
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.HTML.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.HTML.Headers)
	rt := e.runtime.Load()
	maps.Copy(headers, rt.HTML.Headers)

	userAgent := headers["User-Agent"]
	delete(headers, "User-Agent")

	timeout := e.cfg.HTML.Timeout
	maxBody := e.cfg.HTML.MaxBodySize
	if rt.HTML.Timeout != nil {
		timeout = rt.HTML.Timeout.Duration
	}
	if rt.HTML.MaxBodySize != nil {
		maxBody = *rt.HTML.MaxBodySize
	}

	return htmlfetch.Config{
		Timeout:      timeout,
		MaxBodySize:  maxBody,
		UserAgent:    userAgent,
		Headers:      headers,
		ProxyManager: e.GetProxy(),
	}
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

// ApplyRegisterStage 启用注册业务流程开启对应工作池
func (e *Engine) ApplyRegisterStage() {
	for stage, stageInfo := range e.stages {
		cfg := stageInfo.config
		pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize, e.cfg.Crawler.QueueWatermark)
		e.stages[stage].workerPool = pool
		// 启动 worker pool
		pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
			// 认领：把「待处理」置为「处理中」，让运营侧能看出这条归谁管，
			// 也让三个后台动作的状态守卫成立。拿不到行就跳过执行 —— 原因不止一种：
			// 运营已经动过它（标失败/删除），或同一行的另一份副本（后台「加急」会另投一份）先认领了。
			claimed, err := e.claimTask(task)
			if err != nil {
				e.loggerSet.Engine.Errorf("claim task %d: %s", task.ID, err.Error())
			} else if !claimed {
				e.loggerSet.Engine.Warnf("task %d 已被认领或改动（不再是待处理），跳过本次执行", task.ID)
				return nil
			}
			// 重试FetchHandler
			var lastErr error
			for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
				if attempt > 0 {
					task.IncRetry(e.db)
				}
				err := e.runAttempt(ctx, stageInfo.fetcher, task, attempt)
				if err == nil {
					task.UpdateStatus(e.db, models.TaskStatusSuccess, nil)
					if !task.Repeatable {
						e.DelActiveTask(task)
					}
					<-time.After(cfg.Delay.Random())
					return nil
				}
				// 不可重试的错误：直接标 failed，不再空转重试
				if !Retryable(err) {
					task.UpdateStatus(e.db, models.TaskStatusFailed, err)
					e.notifyFailure(ctx, task, err)
					if !task.Repeatable {
						e.DelActiveTask(task)
					}
					return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, err)
				}
				lastErr = err
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(cfg.Backoff * (1 << uint(attempt))):
					continue
				}
			}
			// 所有重试失败：记录错误并更新状态为 failed
			task.UpdateStatus(e.db, models.TaskStatusFailed, lastErr)
			e.notifyFailure(ctx, task, lastErr)
			if !task.Repeatable {
				e.DelActiveTask(task)
			}
			return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, lastErr)
		})
		// 检查提交任务
		if stageInfo.submitFunc != nil {
			stageInfo.submitFunc(e)
		}
		// 监控页面开启时，为该阶段创建统计器并启动
		if e.cfg.Server.Monitor {
			stats := track.NewStatsQueue(pool)
			stats.Start(e.ctx)
			e.setStatsQueue(stage, stats)
			e.loggerSet.Monitor.Infof("monitor started for stage: %s", stage)
		}
	}
	// 启动高水位溢出任务的回灌协程
	e.startDrain()
	// 启动错误队列后台自动轮询（未配置 interval 则不启动，仅手动触发）
	e.startErrorQueue()
	// 启动中断恢复队列（启用时启动即恢复一次 + 定时轮询）
	e.startRecoverQueue()
	// 启动周期轮询队列（repeatable 任务的定时重跑）
	e.startRepeatQueue()
	// 启动步骤追踪的保留期清理（追踪未开启时不启动）
	e.startTraceCleanup()
	// 监控开启时低频采样三队列积压（COUNT 查询，不进监控页请求路径）
	if e.cfg.Server.Monitor {
		e.startQueueSampler()
	}
}

// runAttempt 执行一次 FetchHandler，并负责这一次尝试的步骤追踪：
// 挂记录器 → 跑 handler → 记下结局 → 落库。
//
// 落库放在 defer 里，所以 panic 展开时也会执行 —— 闭包这一层的 defer 先于 workerpool
// 的 recover 跑，panic 之前已上报的步骤因此不会丢；panic 路径上 setResult 没被调用，
// flush 按失败处理、data 保留，正好是排查现场要看的。
// 这里**不 recover**：panic 继续抛给 workerpool，它那边的 debug.Stack() 与 failed
// 计数才是既有行为，重新 panic 反而会丢掉 handler 的栈帧。
func (e *Engine) runAttempt(ctx context.Context, fetcher Fetcher, task *Task, attempt int) error {
	tr := e.newTrace(task, attempt)
	task.Trace = tr
	defer tr.finish() // finish 幂等，正常返回与 panic 展开都走这里

	// 加急记在 trace 的第一步，事后还能看出"这条曾经加急跑过"：
	// claimTask 认领时会把 urgent 列归零（加急是「排队位置」的概念，跑过一次即完成使命），
	// 那张表上就再也看不出它加急过了。只记第一次尝试 —— 同一次执行里的后续重试不是新的加急。
	if attempt == 0 && task.Urgent {
		tr.Step(traceUrgentStep, nil)
	}

	err := fetcher.FetchHandler(ctx, task, e)
	tr.setResult(err)
	return err
}

// logSubmitError 框架自动记录提交类错误，避免业务漏记导致错误丢失；返回原 error 供调用方继续处理。
func (e *Engine) logSubmitError(task *Task, err error) error {
	e.loggerSet.Engine.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	return err
}

// SubmitTask 任务提交
func (e *Engine) SubmitTask(task *Task) error {
	if task.Stage == "" || task.URL == "" {
		return e.logSubmitError(task, fmt.Errorf("task stage or url is empty: %+v", task))
	}
	if _, ok := e.cfg.Crawler.Stages[task.Stage]; !ok {
		return e.logSubmitError(task, fmt.Errorf("invalid stage: %s", task.Stage))
	}

	key := task.Unique()
	var record models.CrawlerTask

	// 两阶段去重：先查内存缓存，miss 再查 DB（唯一索引 idx_stage_url 兜底）
	if e.dedupCache.Get(key) {
		if !task.Repeatable {
			return nil // 去重命中：视为成功，无需重复处理
		}
		// 已入库的轮询任务：查找记录防止重复录入
		if err := e.findTaskRecord(task, &record); err != nil {
			return e.logSubmitError(task, fmt.Errorf("load repeat task record: %w", err))
		}
		task.ID = int(record.ID)
		return e.submitIfActive(task, record)
	}

	err := e.findTaskRecord(task, &record)
	if err == nil {
		// DB 命中：复用已有记录（重新加入内存缓存）
		e.dedupCache.Add(key)
		wasNew := task.ID == 0
		task.ID = int(record.ID)
		// 仅「新任务」去重跳过；已带 ID 的重提交（如 RecoverJob）需继续入队
		if !task.Repeatable && wasNew {
			return nil
		}
		return e.submitIfActive(task, record)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return e.logSubmitError(task, fmt.Errorf("query task dedup: %w", err))
	}

	// 全新任务：提交到 pool 前先插入数据库
	if ierr := task.Insert(e.db); ierr != nil {
		// 并发下撞唯一索引：另一 goroutine 已入库并将入队，此处回填 ID 后跳过，避免重复入队
		if rerr := e.findTaskRecord(task, &record); rerr != nil {
			return e.logSubmitError(task, fmt.Errorf("insert crawler task to db failed: %w", ierr))
		}
		e.dedupCache.Add(key)
		task.ID = int(record.ID)
		return nil
	}
	e.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record)
	e.dedupCache.Add(key)
	return e.submitIfActive(task, record)
}

// findTaskRecord 按去重键查找已存在的任务记录；未找到返回 gorm.ErrRecordNotFound。
// 幂等键优先，否则回退 stage+url（与 DB 唯一索引 idx_stage_url 一致）。
func (e *Engine) findTaskRecord(task *Task, record *models.CrawlerTask) error {
	q := e.db.Model(&models.CrawlerTask{})
	if task.IdempotencyKey != "" {
		q = q.Where("idempotency_key = ?", task.IdempotencyKey)
	} else {
		q = q.Where("url = ? AND stage = ?", task.URL, task.Stage)
	}
	return q.First(record).Error
}

// submitIfActive 若任务尚未到终态（success/failed）则延迟投递或入队，否则静默跳过。
func (e *Engine) submitIfActive(task *Task, record models.CrawlerTask) error {
	if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
		return nil
	}
	// 延迟投递：到点才入队，避免 worker 空等浪费并发位
	if at := task.deliverAt(); !at.IsZero() && at.After(time.Now()) {
		e.enqueueDelayed(at, task, record)
		return nil
	}
	if err := e.submitToPool(task, record); err != nil {
		return e.logSubmitError(task, err)
	}
	return nil
}

// submitToPool 将任务提交到对应阶段工作池，并更新数据库状态。
func (e *Engine) submitToPool(task *Task, record models.CrawlerTask) error {
	info := e.stages[task.Stage]

	// 先落库再入队：「已入队 ⇒ 行里是 pending」必须是不变量。
	// worker 取到任务时会以 status=待处理 为条件认领（claimTask），
	// 如果这里先入队后落库，中间那个窗口里取到任务的 worker（repeatable 重跑时
	// 行里还是"成功"）会被误判成"已被运营改动"而跳过执行。
	if task.Repeatable && task.ID != 0 {
		record.Repeat += 1
	}
	record.Status = models.TaskStatusPending
	e.db.Save(record)

	if err := e.submitTo(info, task); err != nil {
		if errors.Is(err, workerpool.ErrQueueFull) {
			// 队列达 75% 高水位：任务保持 pending（已入库），加入溢出列表由 drain 稍后回灌
			e.spilledCount.Add(1)
			e.spillTask(task)
			return nil
		}
		// 提交失败，回滚内存去重表和数据库状态
		e.dedupCache.Delete(task.Unique())
		record.Error += err.Error() + "\n"
		record.Status = models.TaskStatusFailed
		e.db.Save(&record)
		return err
	}
	return nil
}

// claimTask 把任务从「待处理」认领为「处理中」，成功才允许执行。
// 条件更新而非先读再写：运营在它被 worker 取走之前标了失败（或删了行）时，
// 这里拿不到行，任务就不再执行 —— 这正是「标失败」对排队中任务的拦截力。
// 顺带把 urgent 归零：加急是「排队位置」的概念，跑过一次就完成使命，
// 不清的话失败重投会让加急任务越积越多、快车道被老任务长期占住。
// 返回 (是否认领成功, 错误)：DB 抖动不当成"认领失败"，由调用方记日志后继续执行，
// 免得一次抖动让任务永远没人跑。
func (e *Engine) claimTask(task *Task) (bool, error) {
	if task.ID == 0 {
		return true, nil // 未落库的任务（理论上不该出现）：没有行可认领，直接执行
	}
	res := claimScope(e.db, uint(task.ID)).Updates(map[string]any{
		"status": models.TaskStatusProcessing,
		"urgent": false,
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// submitTo 按任务的加急标记选队列：加急走快车道，其余走常规队列。
// 池子本身不认识优先级（Tasker 接口只有 Unique），路由决定留在这里。
func (e *Engine) submitTo(info *stageInfo, task *Task) error {
	if task.Urgent {
		return info.workerPool.SubmitUrgent(task)
	}
	return info.workerPool.Submit(task)
}

// spillTask 将任务加入高水位溢出列表，等待 drain 重新入队。
func (e *Engine) spillTask(task *Task) {
	e.spillMu.Lock()
	e.spilled[task.Stage] = append(e.spilled[task.Stage], task)
	e.spillMu.Unlock()
}

// startDrain 启动后台回灌协程：定期把溢出列表中的任务重新入队。
func (e *Engine) startDrain() {
	interval := e.cfg.Crawler.DrainInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.drainSpilled()
			}
		}
	}()
}

// drainSpilled 把溢出列表中队列已有空间的任务重新入队；仍满则继续留在列表。
func (e *Engine) drainSpilled() {
	e.spillMu.Lock()
	if len(e.spilled) == 0 {
		e.spillMu.Unlock()
		return
	}
	all := e.spilled
	e.spilled = make(map[string][]*Task)
	e.spillMu.Unlock()

	for stage, tasks := range all {
		info := e.stages[stage]
		if info == nil || info.workerPool == nil {
			continue
		}
		for _, task := range tasks {
			var record models.CrawlerTask
			err := e.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record).Error
			if err != nil {
				if !errors.Is(err, gorm.ErrRecordNotFound) {
					// 瞬态错误：重新加入溢出列表，下轮重试
					e.spillTask(task)
				}
				continue
			}
			if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
				continue // 已终态，无需再入队
			}
			if err := e.submitToPool(task, record); err != nil {
				e.loggerSet.Engine.Errorf("drain spilled task %d: %s", task.ID, err.Error())
			}
		}
	}
}

// SubmitTasks 批量提交任务：一次性批量入库（减少 DB 往返），再逐个入队。
func (e *Engine) SubmitTasks(tasks []*Task) error {
	if len(tasks) == 0 {
		return nil
	}
	for _, t := range tasks {
		if t.Stage == "" || t.URL == "" {
			return e.logSubmitError(t, fmt.Errorf("task stage or url is empty: %+v", t))
		}
		if _, ok := e.cfg.Crawler.Stages[t.Stage]; !ok {
			return e.logSubmitError(t, fmt.Errorf("invalid stage: %s", t.Stage))
		}
	}

	// 两阶段去重与 repeatable 处理，收集需入库/入队的任务
	var toInsert []*Task
	var toProcess []*Task
	for _, t := range tasks {
		key := t.Unique()
		if e.dedupCache.Get(key) {
			if !t.Repeatable {
				continue // 去重命中：跳过
			}
			var record models.CrawlerTask
			if err := e.findTaskRecord(t, &record); err != nil {
				return e.logSubmitError(t, fmt.Errorf("load repeat task record: %w", err))
			}
			t.ID = int(record.ID)
		} else {
			var record models.CrawlerTask
			err := e.findTaskRecord(t, &record)
			switch {
			case err == nil:
				// DB 命中：复用已有记录
				e.dedupCache.Add(key)
				t.ID = int(record.ID)
				if !t.Repeatable {
					continue
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				if t.ID == 0 {
					toInsert = append(toInsert, t)
				}
			default:
				return e.logSubmitError(t, fmt.Errorf("query task dedup: %w", err))
			}
		}
		toProcess = append(toProcess, t)
	}

	// 批量入库（单条多行 INSERT），失败退到逐条
	var conflicted map[string]struct{}
	if len(toInsert) > 0 {
		var err error
		conflicted, err = e.insertTasks(toInsert)
		if err != nil {
			err = fmt.Errorf("insert crawler tasks: %w", err)
			e.loggerSet.Engine.Errorf("submit tasks failed: %v", err)
			return err
		}
	}

	// 逐个入队（含延迟投递）
	var errs []error
	for _, t := range toProcess {
		// 并发撞车的那几条：库里那行已经被另一路入库并入队了，别再入一次（与 SubmitTask 同一处理）
		if _, hit := conflicted[t.Unique()]; hit {
			e.dedupCache.Add(t.Unique())
			continue
		}
		e.dedupCache.Add(t.Unique())
		var record models.CrawlerTask
		e.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).First(&record)
		if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
			continue
		}
		if at := t.deliverAt(); !at.IsZero() && at.After(time.Now()) {
			e.enqueueDelayed(at, t, record)
			continue
		}
		if err := e.submitToPool(t, record); err != nil {
			e.logSubmitError(t, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// insertTasks 批量入库并回填 ID，返回其中「撞了唯一索引、复用了库里已有行」的那些任务的去重键。
//
// 快路径就是一条多行 INSERT —— SubmitTasks 本来就是冲着减少 DB 往返来的。
//
// 但**一行冲突不该让整批（可能上万条）一条都进不去**，而且还直接返回错误、连队列都没进，
// 业务看到的是一整批任务凭空消失。所以批量失败就退到逐条插。
//
// 这里不去分辨"批量失败是不是因为重复键"：逐条那条路自己会分辨 —— 插不进去就按唯一索引
// `idx_stage_url` 回查一下，查得到说明确实是并发撞车（别的 goroutine / 进程在这两步之间
// 把同样的 stage|url 写进去了），回填它的 ID、记进返回的集合；查不到说明插入是真的失败了，
// 原样把插入的那个错报出去（它比"没查到"更有信息量）。
// 这么写也就不依赖 gorm 的 TranslateError（默认没开）去识别 MySQL 的 1062。
func (e *Engine) insertTasks(tasks []*Task) (map[string]struct{}, error) {
	records := make([]models.CrawlerTask, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, t.toModel())
	}

	if err := e.db.Create(&records).Error; err == nil {
		for i, t := range tasks {
			t.ID = int(records[i].ID)
		}
		return nil, nil
	}

	e.loggerSet.Engine.Warnf("batch insert failed, falling back to per-row insert (%d tasks)", len(tasks))
	conflicted := make(map[string]struct{})
	for _, t := range tasks {
		rec := t.toModel()
		ierr := e.db.Create(&rec).Error
		if ierr == nil {
			t.ID = int(rec.ID)
			continue
		}
		// 按 (stage, url) 回查 —— 唯一索引就是这个，所以冲突只可能落在它上面。
		// 不复用 findTaskRecord：它优先按 IdempotencyKey 查，而那不是唯一索引，可能捞回另一行。
		var existed models.CrawlerTask
		if serr := e.db.Select("id").Where("url = ? AND stage = ?", t.URL, t.Stage).
			First(&existed).Error; serr != nil {
			return nil, fmt.Errorf("insert task %q: %w", t.URL, ierr)
		}
		t.ID = int(existed.ID)
		conflicted[t.Unique()] = struct{}{}
	}
	return conflicted, nil
}

// ReSubmitTask 已入库的非轮询任务进行重提交任务
func (e *Engine) ReSubmitTask(task *Task) error {
	if task.Stage == "" || task.URL == "" {
		return e.logSubmitError(task, fmt.Errorf("task stage or url is empty: %+v", task))
	}
	if _, ok := e.cfg.Crawler.Stages[task.Stage]; !ok {
		return e.logSubmitError(task, fmt.Errorf("invalid stage: %s", task.Stage))
	}
	var record models.CrawlerTask
	e.db.Model(&models.CrawlerTask{}).
		Where("url = ?", task.URL).
		Where("stage = ?", task.Stage).
		First(&record)
	if record.ID == 0 {
		return e.logSubmitError(task, fmt.Errorf("record not exists: %s", task.URL))
	}
	task.ID = int(record.ID)
	info := e.stages[task.Stage]
	if err := e.submitTo(info, task); err != nil {
		// 提交失败，回滚内存去重表和数据库状态
		e.dedupCache.Delete(task.Unique())
		record.Error += err.Error() + "\n"
		record.Status = models.TaskStatusFailed
		e.db.Save(&record)
		return e.logSubmitError(task, err)
	}
	return nil
}

// Stop 停止engine
func (e *Engine) Stop(timeout time.Duration) {
	e.cancel()
	var wg sync.WaitGroup
	for name, s := range e.stages {
		wg.Add(1)
		go func(stage string, pool *workerpool.WorkerPool[*Task]) {
			defer wg.Done()
			pool.Stop(timeout)
		}(name, s.workerPool)
	}
	wg.Wait()
	e.loggerSet.Engine.Info("all workers stopped")
}

// Errors 返回work pool中的错误消息队列
func (e *Engine) Errors() (errors []<-chan error) {
	for _, s := range e.stages {
		errors = append(errors, s.workerPool.Errors())
	}
	return
}

// setStatsQueue 设置阶段统计信息管理器
func (e *Engine) setStatsQueue(stage string, mon *track.StatsQueue[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.statsQueue == nil {
		e.statsQueue = make(map[string]*track.StatsQueue[*Task])
	}
	e.statsQueue[stage] = mon
}

// GetStageStats 返回各阶段统计快照（纯值类型，供监控页读取，不暴露内部实现）
func (e *Engine) GetStageStats() map[string]StageStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]StageStats, len(e.statsQueue))
	for stage, mon := range e.statsQueue {
		submitted, completed, failed, inProgress, queueLen := mon.WorkPool.Stats()
		allWorkers := mon.GetAllWorkerStats()
		workers := make(map[int]WorkerStat, len(allWorkers))
		for id, w := range allWorkers {
			workers[id] = WorkerStat(w)
		}
		out[stage] = StageStats{
			Global:  GlobalStats(mon.GetGlobalStats()),
			Workers: workers,
			Queue: QueueStats{
				Submitted:  submitted,
				Completed:  completed,
				Failed:     failed,
				InProgress: inProgress,
				QueueLen:   queueLen,
			},
		}
	}
	return out
}

// DelActiveTask 从去重任务列表中删除任务
func (e *Engine) DelActiveTask(task *Task) {
	e.dedupCache.Delete(task.Unique())
}

// loadActiveTasks 启动时把「进行中」任务（pending/processing）与全部轮询任务加载进内存去重表，
// 避免全表加载导致内存随历史任务无限增长；已完成任务由 DB 唯一索引兜底去重。
func (e *Engine) loadActiveTasks() {
	var tasks []models.CrawlerTask
	err := e.db.Where("status IN ? OR repeatable = ?",
		[]models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing},
		models.RepeatableYes).
		Find(&tasks).Error
	if err != nil {
		e.loggerSet.DB.Errorf("load active tasks failed: %s", err.Error())
		return
	}
	for _, t := range tasks {
		task := Task{URL: t.URL, Stage: t.Stage, IdempotencyKey: t.IdempotencyKey}
		e.dedupCache.Add(task.Unique())
	}
}
