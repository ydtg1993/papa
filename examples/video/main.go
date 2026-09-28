package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/ydtg1993/papa"
	"github.com/ydtg1993/papa/examples/video/fetcher"
)

func main() {
	// 1. 创建应用容器（默认读 configs/config.yaml，可用 papa.WithConfigPath 覆盖）
	app, err := papa.New()
	if err != nil {
		panic(fmt.Sprintf("init app failed: %s", err.Error()))
	}

	// 2. 可选：设置中间件（代理 / m3u8 下载器 / 文件下载器），需在 RegisterStage 之前
	// app.Engine.SetProxy(proxy.NewManager(app.Config.Proxy.APIURL, 8*time.Minute))
	// app.Engine.SetFiledown(filedown.NewDownloader(filedown.DefaultConfig()))
	// app.Engine.SetM3U8(m3u8.NewDownloader(m3u8.DefaultConfig()))

	// 3. 注册爬虫阶段，回调里提交起始任务
	app.RegisterStage(&fetcher.FetchFirst{},
		func(engine *papa.Engine) {
			if err := engine.SubmitTask(&papa.Task{
				PID:        0,
				URL:        app.Config.Crawler.Target + "classify?type=rexue",
				Stage:      "first",
				Repeatable: true,
			}); err != nil {
				app.Logger.Engine.Errorf("submit initial task: %s", err.Error())
			}
		})
	app.RegisterStage(&fetcher.FetchSecond{}, nil)

	// 4. 优雅关闭
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app.Run(ctx)
}
