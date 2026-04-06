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
	ctx         context.Context
	cancel      context.CancelFunc
	stages      map[string]*stageInfo
	activeTasks map[string]bool // hash去重任务表 key: "stage|url"
	activeMu    sync.RWMutex
	mu          sync.RWMutex
	db          *gorm.DB
	cfg         *config.Config
	browserPool *browser.Pool
	statsQueue  map[string]*track.StatsQueue[*Task] // key: stage name 分阶段监控信号
	LoggerSet   *loggers.LoggerSet
	Proxy       *proxy.Manager       // 代理管理器中间件
	M3U8        *m3u8.Downloader     // m3u8下载器
	Filedown    *filedown.Downloader // 文件下载器
}

// stageInfo 内部阶段信息
type stageInfo struct {
	WorkerPool *workerpool.WorkerPool[*Task]
	Config     StageConfig
}

// StageConfig 阶段配置
type StageConfig struct {
	Handler     func(ctx context.Context, task *Task, engine *Engine) error // Handler 是fetcher阶段的核心处理函数
	MaxAttempts int                                                         // Handler 最大重试次数
	Backoff     time.Duration                                               // Handler 初始退避时间
	NextStage   string                                                      // NextStage 可选：解析出的链接自动使用的下一阶段
	WorkerCount int                                                         // WorkerCount 该阶段专用的 worker 数量
	QueueSize   int                                                         // QueueSize 该阶段的任务队列缓冲大小
}

// NewEngine 创建引擎
func NewEngine(pool *browser.Pool, db *gorm.DB, cfg *config.Config, loggerSet *loggers.LoggerSet) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	engine := &Engine{
		ctx:         ctx,
		cancel:      cancel,
		stages:      make(map[string]*stageInfo),
		activeTasks: make(map[string]bool),
		db:          db,
		cfg:         cfg,
		browserPool: pool,
		LoggerSet:   loggerSet,
	}
	engine.loadActiveTasks()
	return engine
}

// SetProxy 设置代理 需要在RegisterStage之前设置
func (e *Engine) SetProxy(proxy *proxy.Manager) {
	e.Proxy = proxy
}

// SetM3U8 设置m3u8下载器 需要在RegisterStage之前设置
func (e *Engine) SetM3U8(m3u *m3u8.Downloader) {
	e.M3U8 = m3u
}

// SetFiledown 设置文件下载器 需要在RegisterStage之前设置
func (e *Engine) SetFiledown(f *filedown.Downloader) {
	e.Filedown = f
}

// RegisterStage 注册一个爬取阶段
func (e *Engine) RegisterStage(f Handler, cfg StageConfig) error {
	//配置检查
	if cfg.WorkerCount <= 0 || cfg.QueueSize <= 0 {
		return fmt.Errorf("stage %s: WorkerCount and QueueSize must be positive", f.GetStage())
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = time.Second
	}

	pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize)
	e.stages[f.GetStage()] = &stageInfo{
		WorkerPool: pool,
		Config:     cfg,
	}
	// 启动 worker pool
	pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
		// 1. 更新状态为 processing
		task.UpdateStatus(e.db, e.LoggerSet.DB, models.TaskStatusProcessing, nil)
		// 2. 重试FetchHandler
		var lastErr error
		for attempt := 0; attempt <= cfg.MaxAttempts; attempt++ {
			if attempt > 0 {
				task.IncRetry(e.db, e.LoggerSet.DB)
			}
			err := f.FetchHandler(ctx, task, e)
			if err == nil {
				task.UpdateStatus(e.db, e.LoggerSet.DB, models.TaskStatusSuccess, nil)
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
		task.UpdateStatus(e.db, e.LoggerSet.DB, models.TaskStatusFailed, lastErr)
		return fmt.Errorf("failed after %d retries: %w", cfg.MaxAttempts, lastErr)
	})
	// 如果全局配置开启了监控，则为该阶段创建监控器并启动
	if e.cfg.Monitor.Enabled {
		stats := track.NewStatsQueue(pool)
		stats.Start()
		e.SetStatsQueue(f.GetStage(), stats)
		e.LoggerSet.Engine.Infof("monitor started for stage: %s", f.GetStage())
	}
	return nil
}

// SubmitTask 任务提交
func (e *Engine) SubmitTask(task *Task, insertToDB bool) error {
	if task.Stage == "" || task.URL == "" {
		return fmt.Errorf("task stage or url is empty: %v", task)
	}
	//加入去重hash map
	e.activeMu.RLock()
	if e.activeTasks[task.Unique()] {
		e.activeMu.RUnlock()
		return fmt.Errorf("task already exists: %s", task.Unique())
	}
	e.activeMu.RUnlock()
	// 提交到 pool 前先插入数据库（避免并发竞争）
	if insertToDB && !task.Insert(e.db, e.LoggerSet.DB) {
		return fmt.Errorf("insert crawler task to db failed")
	}
	// 插入成功后加入内存 map
	e.activeMu.Lock()
	e.activeTasks[task.Unique()] = true
	e.activeMu.Unlock()

	info := e.stages[task.Stage]
	if err := info.WorkerPool.Submit(task); err != nil {
		// 提交失败，回滚内存 map 和数据库状态
		e.activeMu.Lock()
		delete(e.activeTasks, task.Unique())
		e.activeMu.Unlock()
		_ = task.UpdateStatus(e.db, e.LoggerSet.DB, models.TaskStatusFailed, err)
		return err
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
		}(name, s.WorkerPool)
	}
	wg.Wait()
	e.LoggerSet.Engine.Info("all workers stopped")
}

// Errors 返回work pool中的错误消息队列
func (e *Engine) Errors(stage string) <-chan error {
	for name, s := range e.stages {
		if name == stage {
			return s.WorkerPool.Errors()
		}
	}
	panic(fmt.Sprintf("stage %s not found", stage))
}

// GetWorkerPool 返回指定阶段的 worker 池，用于监控等
func (e *Engine) GetWorkerPool(stage string) (*workerpool.WorkerPool[*Task], error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	info, ok := e.stages[stage]
	if !ok {
		return nil, fmt.Errorf("stage %s not found", stage)
	}
	return info.WorkerPool, nil
}

// GetBrowserPool 获取浏览器池
func (e *Engine) GetBrowserPool() *browser.Pool {
	return e.browserPool
}

// SetStatsQueue 设置阶段统计信息管理器
func (e *Engine) SetStatsQueue(stage string, mon *track.StatsQueue[*Task]) {
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

// RecoverTasks 从数据库加载未完成的任务并重新提交
func (e *Engine) RecoverTasks() {
	// 1. 查询需要恢复的任务（pending 或超时的 processing）
	var tasks []models.CrawlerTask
	timeout := time.Now().Add(-2 * time.Hour)

	if err := e.db.Where("(status = ? OR status = ?) AND updated_at < ?",
		models.TaskStatusPending, models.TaskStatusProcessing, timeout).
		Find(&tasks).Error; err != nil {
		e.LoggerSet.DB.Errorf("failed to load tasks for recovery: %v", err)
		return
	}

	if len(tasks) == 0 {
		e.LoggerSet.Engine.Info("no tasks need recovery")
		return
	}

	e.LoggerSet.Engine.Infof("found %d tasks need to recover", len(tasks))

	// 2. 分别记录提交成功和失败的任务 ID
	var successIDs []uint
	var failIDs []uint
	for _, t := range tasks {
		task := &Task{
			ID:    int64(t.ID),
			URL:   t.URL,
			Stage: t.Stage,
			Retry: t.Retry,
		}
		// 剔除去重hash map的暂存
		delete(e.activeTasks, task.Unique())
		// 重新提交到队列（非阻塞）
		if err := e.SubmitTask(task, false); err != nil {
			e.LoggerSet.Engine.Errorf("recover task %d (url: %s) submit failed: %v",
				t.ID, t.URL, err)
			failIDs = append(failIDs, t.ID)
		} else {
			successIDs = append(successIDs, t.ID)
			e.LoggerSet.Engine.Infof("recover task %d submitted successfully", t.ID)
		}
	}

	// 3. 批量更新成功任务的状态为 Processing
	if len(successIDs) > 0 {
		if err := e.db.Model(&models.CrawlerTask{}).
			Where("id IN ?", successIDs).
			Update("status", models.TaskStatusProcessing).Error; err != nil {
			e.LoggerSet.DB.Errorf("failed to mark recovered tasks as processing: %v", err)
		} else {
			e.LoggerSet.Engine.Infof("marked %d tasks as processing", len(successIDs))
		}
	}

	// 4. 失败的任务状态回滚为 Pending（记录为失败进入失败任务流程）
	if len(failIDs) > 0 {
		if err := e.db.Model(&models.CrawlerTask{}).
			Where("id IN ?", failIDs).
			Update("status", models.TaskStatusFailed).
			Update("error", "RecoverTasks 恢复任务提交队列失败").Error; err != nil {
			e.LoggerSet.DB.Errorf("failed to rollback failed tasks to failed: %v", err)
		} else {
			e.LoggerSet.Engine.Warnf("rolled back %d tasks to pending due to submit failure", len(failIDs))
		}
	}
}

// loadActiveTasks 启动时加载已处理任务到去重hash map
func (e *Engine) loadActiveTasks() {
	var tasks []models.CrawlerTask
	if err := e.db.Where("status IN (?)",
		[]models.TaskStatus{
			models.TaskStatusPending,
			models.TaskStatusProcessing,
			models.TaskStatusSuccess,
			models.TaskStatusFailed}).Find(&tasks).Error; err != nil {
		e.LoggerSet.DB.Errorf("load active tasks failed: %v", err)
		return
	}
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	for _, t := range tasks {
		task := Task{URL: t.URL, Stage: t.Stage}
		e.activeTasks[task.Unique()] = true
	}
}
