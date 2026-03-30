package main

import (
	"github.com/chromedp/chromedp"
	"github.com/ydtg1993/papa/internal/config"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/fetcher"
	"github.com/ydtg1993/papa/internal/loggers"
	_ "github.com/ydtg1993/papa/internal/loggers"
	"github.com/ydtg1993/papa/pkg/database"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 1. 加载配置
	err := config.Load("configs/config.yaml")
	if err != nil {
		loggers.SysLogger.Fatal("load config failed:", err)
	}
	// 2.加载数据库
	err = database.NewDB(&config.Cfg.DB)
	if err != nil {
		loggers.DBLogger.WithError(err).Fatal("connect database failed")
	}
	defer func() {
		sqlDB, _ := database.DB.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}()
	// 3. 创建引擎
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
	// 4. 注册爬取阶段
	fetchCatalog := &fetcher.FetchCatalog{}
	err = engine.RegisterStage(fetchCatalog,
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
	//启动错误收集
	go func() {
		for err := range engine.Errors(fetchCatalog.GetStage()) {
			loggers.EngineLogger.Printf("crawler catalog error: %v", err)
		}
	}()

	// 6. 等待退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	loggers.SysLogger.Info("shutting down gracefully...")
	engine.Stop(time.Second * 5)
}
