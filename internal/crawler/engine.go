package crawler

import (
	"context"
	"fmt"
	"github.com/chromedp/chromedp"
	"github.com/ydtg1993/papa/internal/loggers"
	"github.com/ydtg1993/papa/internal/middleware/proxy"
	"github.com/ydtg1993/papa/pkg/browser"
	"github.com/ydtg1993/papa/pkg/monitor"
	"github.com/ydtg1993/papa/pkg/workerpool"
	"sync"
	"time"
)

type Engine struct {
	monitor     *monitor.Monitor[*Task]
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
	// Handler 是阶段的核心处理函数，如果为 nil 则使用默认的 DefaultHandler
	Handler func(ctx context.Context, task *Task, engine *Engine) error
	// NextStage 可选：解析出的链接自动使用的下一阶段
	NextStage string
	// WorkerCount 该阶段专用的 worker 数量
	WorkerCount int
	// QueueSize 该阶段的任务队列缓冲大小
	QueueSize int
}

type Handler interface {
	GetStage() string
	FetchHandler(ctx context.Context, task *Task, engine *Engine) error
}

// NewEngine 创建引擎
func NewEngine(bCfg BrowserConfig) *Engine {
	browserPool, err := browser.NewPool(bCfg.Size, bCfg.Opts, bCfg.MaxIdleTime)
	if err != nil {
		loggers.EngineLogger.Printf("crawler new browser pool error: %v", err)
	}
	engine := &Engine{
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
	if cfg.WorkerCount <= 0 || cfg.QueueSize <= 0 {
		return fmt.Errorf("stage %s: WorkerCount and QueueSize must be positive", f.GetStage())
	}
	pool := workerpool.NewWorkerPool[*Task](cfg.WorkerCount, cfg.QueueSize)
	e.stages[f.GetStage()] = &stageInfo{
		WorkerPool: pool,
		Config:     cfg,
	}
	// 启动该阶段的 worker
	pool.Start(func(ctx context.Context, task *Task) error {
		return f.FetchHandler(ctx, task, e)
	})
	return nil
}

func (e *Engine) SubmitTask(stage string, task *Task) error {
	e.mu.RLock()
	info, ok := e.stages[stage]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("stage %s not found", stage)
	}
	return info.WorkerPool.Submit(task)
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
}

func (e *Engine) Errors(stage string) <-chan error {
	for name, s := range e.stages {
		if name == stage {
			return s.WorkerPool.Errors()
		}
	}
	panic(fmt.Sprintf("stage %s not found", stage))
}
