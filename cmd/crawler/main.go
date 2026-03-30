package main

import (
	"github.com/chromedp/chromedp"
	_ "github.com/ydtg1993/papa/internal"
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/fetcher"
	"github.com/ydtg1993/papa/internal/loggers"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 1. 创建引擎
	engine := crawler.NewEngine(crawler.BrowserConfig{
		Size: 2,
		Opts: []chromedp.ExecAllocatorOption{
			chromedp.Flag("headless", false), // 显示浏览器窗口
			chromedp.Flag("window-size", "1280,720"),
			chromedp.Flag("disable-gpu", false),
			chromedp.Flag("no-sandbox", false),
		},
		MaxIdleTime: time.Minute * 8,
		//设置代理
		//ProxyMgr: proxy.NewManager(config.Cfg.Proxy.APIURL, config.Cfg.Proxy.RefreshInterval),
	})
	// 2. 注册爬取阶段
	fetchCatalog := &fetcher.FetchCatalog{}
	err := engine.RegisterStage(fetchCatalog,
		crawler.StageConfig{
			WorkerCount: config.Cfg.Crawler.Stages["catalog"].WorkerCount,
			QueueSize:   config.Cfg.Crawler.Stages["catalog"].QueueSize,
			NextStage:   "detail",
		})
	if err != nil {
		loggers.EngineLogger.Fatal("register catalog stage failed:", err)
	}
	_ = engine.SubmitTask("catalog", &crawler.Task{
		URL:   "https://hanime.tv/home",
		Stage: fetchCatalog.GetStage(),
	})
	//恢复未完成任务
	engine.RecoverTasks()

	//启动任务监控
	go func() {
		for err := range engine.Errors(fetchCatalog.GetStage()) {
			loggers.EngineLogger.Printf("crawler catalog error: %v", err)
		}
	}()

	/*mon := monitor.NewMonitor(engine.("catalog"))
	mon.Start()
	defer mon.Stop()*/

	// 6. 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	loggers.SysLogger.Info("shutting down gracefully...")
	engine.Stop(time.Second * 5)
}
