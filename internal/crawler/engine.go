package crawler

import (
	"context"
	"fmt"
	"github.com/chromedp/chromedp"
	"github.com/ydtg1993/papa/internal/loggers"
	"github.com/ydtg1993/papa/internal/middleware/proxy"
	"github.com/ydtg1993/papa/internal/models"
	"github.com/ydtg1993/papa/pkg/browser"
	"github.com/ydtg1993/papa/pkg/database"
	"github.com/ydtg1993/papa/pkg/workerpool"
	"sync"
	"time"
)

type Engine struct {
	ctx         context.Context
	cancel      context.CancelFunc
	BrowserPool *browser.Pool
	stages      map[string]*stageInfo
	mu          sync.RWMutex
}

type WorkerConfig struct {
	Workers        int
	QueueSize      int
	StopTimeout    time.Duration
	RequestTimeout time.Duration
}

type BrowserConfig struct {
	Size        int
	Opts        []chromedp.ExecAllocatorOption
	MaxIdleTime time.Duration
	ProxyMgr    *proxy.Manager
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
func NewEngine(bCfg BrowserConfig) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	browserPool, err := browser.NewPool(bCfg.Size, bCfg.Opts, bCfg.MaxIdleTime)
	if err != nil {
		loggers.EngineLogger.Printf("crawler new browser pool error: %v", err)
		panic("crawler new browser pool error")
	}
	engine := &Engine{
		ctx:         ctx,
		cancel:      cancel,
		BrowserPool: browserPool,
		stages:      make(map[string]*stageInfo),
	}
	if bCfg.ProxyMgr != nil {
		browserPool.SetProxyMgr(bCfg.ProxyMgr)
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
	pool.Start(e.ctx, func(ctx context.Context, task *Task) error {
		// 1. 更新状态为 processing
		task.UpdateStatus(models.TaskStatusProcessing, nil)
		// 2. 重试FetchHandler
		var lastErr error
		for attempt := 0; attempt <= cfg.MaxAttempts; attempt++ {
			if attempt > 0 {
				task.IncRetry()
			}
			err := f.FetchHandler(ctx, task, e)
			if err == nil {
				task.UpdateStatus(models.TaskStatusSuccess, nil)
				return nil
			}
			lastErr = err
			select {
			case <-time.After(cfg.Backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// 所有重试失败：记录错误并更新状态为 failed
		task.UpdateStatus(models.TaskStatusFailed, lastErr)
		return fmt.Errorf("failed after %d retries: %w", cfg.MaxAttempts, lastErr)
	})
	return nil
}

// recordError 记录错误到数据库
func (e *Engine) recordError(task *Task, stage string, err error) error {
	crawlerTask := &models.CrawlerTask{
		URL:   task.URL,
		Stage: stage,
		Error: err.Error(),
		Retry: task.Retry,
	}
	return database.DB.Create(crawlerTask).Error
}

func (e *Engine) SubmitTask(stage string, task *Task) error {
	// 1. 写入数据库
	dbTask := &models.CrawlerTask{
		URL:    task.URL,
		Stage:  stage,
		Retry:  0,
		Status: models.TaskStatusPending,
	}
	if err := database.DB.Create(dbTask).Error; err != nil {
		return fmt.Errorf("save task to db failed: %v", err)
	}
	task.ID = int64(dbTask.ID) // 记录数据库 ID
	if err := e.submitToPool(stage, task); err != nil {
		task.UpdateStatus(models.TaskStatusFailed, err)
		return err
	}
	return nil
}

func (e *Engine) Stop(timeout time.Duration) {
	var wg sync.WaitGroup
	for name, s := range e.stages {
		wg.Add(1)
		go func(stage string, pool *workerpool.WorkerPool[*Task]) {
			defer wg.Done()
			if err := pool.Stop(timeout); err != nil {
				loggers.EngineLogger.Printf("stop stage %s error: %v", stage, err)
			}
		}(name, s.WorkerPool)
	}
	wg.Wait()

	if e.BrowserPool != nil {
		e.BrowserPool.Close()
	}
	sqlDB, _ := database.DB.DB()
	if sqlDB != nil {
		_ = sqlDB.Close()
	}
}

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

// RecoverTasks 从数据库加载未完成的任务并重新提交
func (e *Engine) RecoverTasks() {
	var tasks []models.CrawlerTask
	// 查询所有 pending 状态，以及处理中但超过 10 分钟未完成的任务
	timeout := time.Now().Add(-10 * time.Minute)
	database.DB.Where("status IN (?) OR (status = ? AND updated_at < ?)",
		[]models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing},
		models.TaskStatusProcessing, timeout).Find(&tasks)

	loggers.SysLogger.Infof("found %d tasks to recover", len(tasks))

	for _, t := range tasks {
		// 重置状态为 pending（如果是 processing 且超时）
		if t.Status == models.TaskStatusProcessing {
			database.DB.Model(&t).Update("status", models.TaskStatusPending)
		}
		task := &Task{
			ID:    int64(t.ID),
			URL:   t.URL,
			Stage: t.Stage,
			Retry: t.Retry,
		}
		// 重新提交
		if err := e.submitToPool(t.Stage, task); err != nil {
			loggers.EngineLogger.Errorf("recover task %d failed: %v", t.ID, err)
		}
	}
}

func (e *Engine) submitToPool(stage string, task *Task) error {
	e.mu.RLock()
	info, ok := e.stages[stage]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("stage %s not found", stage)
	}
	return info.WorkerPool.Submit(task)
}
