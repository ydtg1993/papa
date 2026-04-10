package crawler

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/models"
	"github.com/ydtg1993/papa/pkg/browser"
	"github.com/ydtg1993/papa/pkg/loggers"
	"github.com/ydtg1993/papa/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/pkg/middleware/proxy"
	"github.com/ydtg1993/papa/pkg/track"
	"github.com/ydtg1993/papa/pkg/workerpool"
	"gorm.io/gorm"
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
	statsQueue  map[string]*track.StatsQueue[*Task] // key: stage name 分阶段监控信号
	activeTasks sync.Map                            // hash去重任务表 key: "stage|url"
	repeatTasks sync.Map                            // 重复轮询任务

	proxy    *proxy.Manager       // 代理管理器中间件
	m3u8     *m3u8.Downloader     // m3u8下载器
	filedown *filedown.Downloader // 文件下载器
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
	MaxAttempts int           // Handler 最大重试次数
	Backoff     time.Duration // Handler 错误重试退避时间
	Delay       time.Duration // 任务间隔延迟
	WorkerCount int           // WorkerCount 该阶段专用的 worker 数量
	QueueSize   int           // QueueSize 该阶段的任务队列缓冲大小
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
	}
	engine.loadActiveTasks()
	return engine
}

// GetDB 获取数据库操作实例
func (e *Engine) GetDB() *gorm.DB {
	return e.db
}

// GetLoggerSet 获取日志管理器列表
func (e *Engine) GetLoggerSet() *loggers.LoggerSet {
	return e.loggerSet
}

// GetConfig 获取全局配置
func (e *Engine) GetConfig() *config.Config {
	return e.cfg
}

// SetBrowserPool 创建浏览器操作池
func (e *Engine) SetBrowserPool() {
	pool, err := browser.NewPool(browser.PoolConfig{
		Size:        e.cfg.Browser.PoolSize,
		MaxIdleTime: e.cfg.Browser.MaxIdleTime,
		Headless:    e.cfg.Browser.Headless,
		NoSandbox:   e.cfg.Browser.NoSandbox,
		BrowserPath: e.cfg.Browser.BrowserPath,
		Flags:       map[string]string{},
		DefaultHeaders: map[string]string{
			"User-Agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8",
			"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
		},
		ProxyManager: e.GetProxy(),
	})
	if err != nil {
		e.loggerSet.Browser.Errorf("new browser pool: %v", err)
	}
	e.browserPool = pool
}

// GetBrowserPool 获取浏览器池
func (e *Engine) GetBrowserPool() *browser.Pool {
	return e.browserPool
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
			for attempt := 0; attempt <= cfg.MaxAttempts; attempt++ {
				if attempt > 0 {
					task.IncRetry(e.db, e.loggerSet.DB)
				}
				err := stageInfo.fetcher.FetchHandler(ctx, task, e)
				if err == nil {
					task.UpdateStatus(e.db, e.loggerSet.DB, models.TaskStatusSuccess, nil)
					<-time.After(cfg.Delay)
					return nil
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
			task.UpdateStatus(e.db, e.loggerSet.DB, models.TaskStatusFailed, lastErr)
			return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, lastErr)
		})
		// 检查提交任务
		if stageInfo.submitFunc != nil {
			stageInfo.submitFunc(e)
		}
		// 如果全局配置开启了监控，则为该阶段创建监控器并启动
		if e.cfg.Monitor.Enabled {
			stats := track.NewStatsQueue(pool)
			stats.Start(e.ctx)
			e.setStatsQueue(stage, stats)
			e.loggerSet.Monitor.Infof("monitor started for stage: %s", stage)
		}
	}
}

// SubmitTask 任务提交
func (e *Engine) SubmitTask(task *Task) error {
	if task.Stage == "" || task.URL == "" {
		return fmt.Errorf("task stage or url is empty: %v", task)
	}
	if _, ok := e.cfg.Crawler.Stages[task.Stage]; ok != true {
		return fmt.Errorf("invalid stage : %s", task.Stage)
	}
	if task.Repeatable {
		//存入轮询任务列表 在任务计划中读取调用
		e.repeatTasks.Store(task.Unique(), task)
	}
	var record models.CrawlerTask
	//查询去重hash map
	_, exist := e.activeTasks.Load(task.Unique())
	if exist {
		if task.Repeatable == false {
			return fmt.Errorf("task already exists: %s", task.Unique())
		} else {
			//已经入库的轮询任务 查找记录防止重复录入
			e.db.Model(&models.CrawlerTask{}).
				Where("url = ?", task.URL).
				Where("stage = ?", task.Stage).
				First(&record)
			task.ID = int(record.ID)
		}
	}

	if task.ID == 0 {
		// 提交到 pool 前先插入数据库
		if task.Insert(e.db, e.loggerSet.DB) == false {
			return fmt.Errorf("insert crawler task to db failed")
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
	info := e.stages[task.Stage]
	if err := info.workerPool.Submit(task); err != nil {
		// 提交失败，回滚内存 map 和数据库状态
		e.activeTasks.Delete(task.Unique())
		record.Error += err.Error() + "\r"
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

// GetStatsQueue 获取全部统计信息控制器
func (e *Engine) GetStatsQueue() map[string]*track.StatsQueue[*Task] {
	e.mu.RLock()
	defer e.mu.RUnlock()
	// 返回副本
	cp := make(map[string]*track.StatsQueue[*Task], len(e.statsQueue))
	for k, v := range e.statsQueue {
		cp[k] = v
	}
	return cp
}

// DelActiveTask 从去重任务列表中删除任务
func (e *Engine) DelActiveTask(task *Task) {
	e.activeTasks.Delete(task.Unique())
}

// GetRepeatTasks 获取轮询任务列表
func (e *Engine) GetRepeatTasks() *sync.Map {
	return &e.repeatTasks
}

// loadActiveTasks 启动时加载数据库记录到去重hash map
func (e *Engine) loadActiveTasks() {
	var tasks []models.CrawlerTask
	if err := e.db.Where("id > ?", 0).Find(&tasks).Error; err != nil {
		e.loggerSet.DB.Errorf("load active tasks failed: %v", err)
		return
	}
	for _, t := range tasks {
		task := Task{URL: t.URL, Stage: t.Stage}
		e.activeTasks.Store(task.Unique(), true)
	}
}
