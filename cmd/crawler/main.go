package main

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/internal/app"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/fetcher"
	"github.com/ydtg1993/papa/pkg/middleware/proxy"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 1. 创建应用容器
	appInstance, err := app.NewApp()
	if err != nil {
		panic(fmt.Sprintf("init app failed: %v", err))
	}
	// 2. 创建可取消的 context，用于优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	//=========================设置爬虫middleware代理,m3u8下载器等=========================//
	appInstance.Engine.SetProxy(proxy.NewManager(appInstance.Config.Proxy.APIURL, 8*time.Minute))

	//=========================爬虫具体业务相关=========================//
	// 3. 注册业务逻辑所需要的爬虫流程阶段
	// stage 需要填入预先配置config.yaml crawler.stages的阶段并适用其配置
	// 阶段一: 抓取分类目录页(需要做一次手动提交将目录页url传入任务) 采集:url 标题 分类信息
	appInstance.RegisterStage("catalog", &fetcher.FetchCatalog{},
		func(engine *crawler.Engine) {
			// 手动提交起始任务 任务需标注下一个阶段为流水作业需要 一般用于初期目录阶段后续任务分发均由fetcher步骤里具体实现
			if err := engine.SubmitTask(&crawler.Task{
				PID:        0, //初级任务
				URL:        appInstance.Config.Crawler.Target + "classify?type=rexue",
				Stage:      "catalog",
				Repeatable: true, //可重复抓取，为后期轮询标识
			}); err != nil {
				appInstance.Logger.Engine.Errorf("submit initial task: %v", err)
			}
		})
	// 阶段二: 抓取详情目录页内容 采集:动漫封面,更新时间,简介,选集内容列表
	appInstance.RegisterStage("detail", &fetcher.FetchDetail{}, nil)
	// 阶段二: 抓取详情目录页内容 采集:动漫视频
	//appInstance.RegisterStage("video", &fetcher.FetchCatalog{}, nil)
	//=========================爬虫具体业务相关=========================//

	// 3. 捕获退出信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel() // 收到信号时取消 context
	}()

	// 4. 运行应用（阻塞直到 ctx，内部自动清理资源）
	appInstance.Run(ctx)
}
