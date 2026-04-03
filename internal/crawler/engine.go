package crawler

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/loggers"
	"github.com/ydtg1993/papa/internal/models"
	"github.com/ydtg1993/papa/pkg/browser"
	"github.com/ydtg1993/papa/pkg/monitor"
	"github.com/ydtg1993/papa/pkg/workerpool"
	"gorm.io/gorm"
	"sync"
	"time"
)

type Engine struct {
	ctx         context.Context
	cancel      context.CancelFunc
	stages      map[string]*stageInfo
	mu          sync.RWMutex
	db          *gorm.DB
	cfg         *config.Config
	browserPool *browser.Pool
	monitors    map[string]*monitor.Monitor[*Task] // key: stage name 分阶段监控器组
	LoggerSet   *loggers.LoggerSet
}

type WorkerConfig struct {
	Workers        int
	QueueSize      int
	StopTimeout    time.Duration
	RequestTimeout time.Duration
}

// stageInfo 内部阶段信息
type stageInfo struct {
	WorkerPool *workerpool.WorkerPool[*Task]
	Config     StageConfig
}

// StageConfig 阶段配置
type StageConfig struct {
	Handler     func(ctx context.Context, task *Task, engine *Engine) error // Handler 是fetcher阶段的核心处理函数
	MaxAttempts int                                                         // Handler最大重试次数
	Backoff     time.Duration                                               // Handler初始退避时间
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
		db:          db,
		LoggerSet:   loggerSet,
		cfg:         cfg,
		browserPool: pool,
	}
	return engine
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
			case <-time.After(cfg.Backoff):
				continue
			}
		}
		// 所有重试失败：记录错误并更新状态为 failed
		task.UpdateStatus(e.db, e.LoggerSet.DB, models.TaskStatusFailed, lastErr)
		return fmt.Errorf("failed after %d retries: %w", cfg.MaxAttempts, lastErr)
	})
	// 如果全局配置开启了监控，则为该阶段创建监控器并启动
	if e.cfg.Monitor.Enabled {
		mon := monitor.NewMonitor(pool)
		mon.Start()
		e.SetMonitor(f.GetStage(), mon)
		e.LoggerSet.Engine.Infof("monitor started for stage: %s", f.GetStage())
	}
	return nil
}

// SubmitTask 任务提交
func (e *Engine) SubmitTask(task *Task) error {
	if task.Stage == "" || task.URL == "" {
		return fmt.Errorf("task stage or url is empty: %v", task)
	}
	if err := e.submitToPool(task); err != nil {
		return err
	}
	if !task.Insert(e.db, e.LoggerSet.DB) {
		return fmt.Errorf("insert crawler task to db failed")
	}
	return nil
}

func (e *Engine) submitToPool(task *Task) error {
	e.mu.RLock()
	info, ok := e.stages[task.Stage]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("stage %s not found", task.Stage)
	}
	return info.WorkerPool.Submit(task)
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

// SetMonitor 设置阶段监控器
func (e *Engine) SetMonitor(stage string, mon *monitor.Monitor[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.monitors == nil {
		e.monitors = make(map[string]*monitor.Monitor[*Task])
	}
	e.monitors[stage] = mon
}

// GetMonitors 获取全部监控器
func (e *Engine) GetMonitors() map[string]*monitor.Monitor[*Task] {
	e.mu.RLock()
	defer e.mu.RUnlock()
	// 返回副本
	cp := make(map[string]*monitor.Monitor[*Task], len(e.monitors))
	for k, v := range e.monitors {
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

		// 尝试提交到队列（非阻塞）
		if err := e.submitToPool(task); err != nil {
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
