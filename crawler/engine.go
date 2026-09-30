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
	browserPool *browser.Pool
	htmlClient  *htmlfetch.Client
	statsQueue  map[string]*track.StatsQueue[*Task] // key: stage name 分阶段监控信号
	activeTasks sync.Map                            // hash去重任务表 key: "stage|url"
	repeatTasks sync.Map                            // 重复轮询任务

	delayMu   sync.Mutex // 保护 delayHeap
	delayHeap delayHeap  // 延迟投递最小堆
	delayCh   chan struct{}

	errorQueueMu sync.Mutex // 串行化错误队列处理，避免自动+手动并发重复投递

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
		ctx:         ctx,
		cancel:      cancel,
		stages:      make(map[string]*stageInfo),
		activeTasks: sync.Map{},
		db:          db,
		cfg:         cfg,
		loggerSet:   loggerSet,
		metrics:     metrics.New(),
		delayCh:     make(chan struct{}, 1),
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

// GetMetrics 返回业务自定义监控数据快照（纯值类型，供监控页读取）
func (e *Engine) GetMetrics() map[string]any {
	if e.metrics == nil {
		return map[string]any{}
	}
	return e.metrics.GetAll()
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
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.Browser.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.Browser.Headers)

	pool, err := browser.NewPool(browser.PoolConfig{
		Size:           e.cfg.Browser.PoolSize,
		DirectSize:     e.cfg.Browser.DirectSize,
		MaxIdleTime:    e.cfg.Browser.MaxIdleTime,
		Headless:       e.cfg.Browser.Headless,
		NoSandbox:      e.cfg.Browser.NoSandbox,
		Leakless:       e.cfg.Browser.Leakless,
		BrowserPath:    e.cfg.Browser.BrowserPath,
		Flags:          map[string]string{},
		DefaultHeaders: headers,
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
	headers := make(map[string]string, len(defaultHeaders)+len(e.cfg.HTML.Headers))
	maps.Copy(headers, defaultHeaders)
	maps.Copy(headers, e.cfg.HTML.Headers)

	userAgent := headers["User-Agent"]
	delete(headers, "User-Agent")

	e.htmlClient = htmlfetch.NewClient(htmlfetch.Config{
		Timeout:      e.cfg.HTML.Timeout,
		MaxBodySize:  e.cfg.HTML.MaxBodySize,
		UserAgent:    userAgent,
		Headers:      headers,
		ProxyManager: e.GetProxy(),
	})
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
		pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize)
		e.stages[stage].workerPool = pool
		// 启动 worker pool
		pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
			// 重试FetchHandler
			var lastErr error
			for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
				if attempt > 0 {
					task.IncRetry(e.db)
				}
				err := stageInfo.fetcher.FetchHandler(ctx, task, e)
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
	// 启动错误队列后台自动轮询（未配置 interval 则不启动，仅手动触发）
	e.startErrorQueue()
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
	if task.Repeatable {
		//存入轮询任务列表 在任务计划中读取调用
		e.repeatTasks.Store(task.Unique(), task)
	}
	var record models.CrawlerTask
	//查询去重hash map
	_, exist := e.activeTasks.Load(task.Unique())
	if exist {
		if !task.Repeatable {
			return nil // 去重命中：视为成功，无需重复处理
		}
		//已经入库的轮询任务 查找记录防止重复录入
		e.db.Model(&models.CrawlerTask{}).
			Where("url = ?", task.URL).
			Where("stage = ?", task.Stage).
			First(&record)
		task.ID = int(record.ID)
	}

	if task.ID == 0 {
		// 提交到 pool 前先插入数据库
		if err := task.Insert(e.db); err != nil {
			return e.logSubmitError(task, fmt.Errorf("insert crawler task to db failed: %w", err))
		}
	}
	// 插入成功后加入内存去重map
	e.activeTasks.Store(task.Unique(), true)
	if record.ID == 0 {
		e.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record)
	}
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
	if err := info.workerPool.Submit(task); err != nil {
		// 提交失败，回滚内存 map 和数据库状态
		e.activeTasks.Delete(task.Unique())
		record.Error += err.Error() + "\n"
		record.Status = models.TaskStatusFailed
		e.db.Save(&record)
		return err
	}
	if task.Repeatable && task.ID != 0 {
		//状态修改 repeat+1
		record.Repeat += 1
		record.Status = models.TaskStatusPending
	} else {
		record.Status = models.TaskStatusPending
	}
	e.db.Save(record)
	return nil
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

	// 去重与 repeatable 处理，收集需入库/入队的任务
	var toInsert []*Task
	var toProcess []*Task
	for _, t := range tasks {
		if t.Repeatable {
			e.repeatTasks.Store(t.Unique(), t)
		}
		if _, exist := e.activeTasks.Load(t.Unique()); exist {
			if !t.Repeatable {
				continue // 去重命中：跳过
			}
			var record models.CrawlerTask
			e.db.Model(&models.CrawlerTask{}).
				Where("url = ?", t.URL).
				Where("stage = ?", t.Stage).
				First(&record)
			t.ID = int(record.ID)
		}
		if t.ID == 0 {
			toInsert = append(toInsert, t)
		}
		toProcess = append(toProcess, t)
	}

	// 批量入库（单条多行 INSERT）
	if len(toInsert) > 0 {
		records := make([]models.CrawlerTask, 0, len(toInsert))
		for _, t := range toInsert {
			records = append(records, t.toModel())
		}
		if err := e.db.Create(&records).Error; err != nil {
			err = fmt.Errorf("insert crawler tasks: %w", err)
			e.loggerSet.Engine.Errorf("submit tasks failed: %v", err)
			return err
		}
		for i, t := range toInsert {
			t.ID = int(records[i].ID)
		}
	}

	// 逐个入队（含延迟投递）
	var errs []error
	for _, t := range toProcess {
		e.activeTasks.Store(t.Unique(), true)
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
	if err := info.workerPool.Submit(task); err != nil {
		// 提交失败，回滚内存 map 和数据库状态
		e.activeTasks.Delete(task.Unique())
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
	e.activeTasks.Delete(task.Unique())
}

// GetRepeatTasks 获取轮询任务列表
func (e *Engine) GetRepeatTasks() *sync.Map {
	return &e.repeatTasks
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
		e.activeTasks.Store(task.Unique(), true)
	}
}
