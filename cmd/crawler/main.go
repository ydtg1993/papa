package main

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/internal/app"
	"github.com/ydtg1993/papa/internal/crawler"
	"github.com/ydtg1993/papa/internal/fetcher"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// 1. 创建应用容器
	appInstance, err := app.NewApp()
	if err != nil {
		panic(fmt.Sprintf("init app failed: %s", err.Error()))
	}
	// 2. 创建可取消的 context，用于优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	//=========================选择设置爬虫middleware代理,m3u8下载器等=========================//
	//appInstance.Engine.SetProxy(proxy.NewManager(appInstance.Config.Proxy.APIURL, 8*time.Minute))
	//appInstance.Engine.SetFiledown(filedown.NewDownloader(filedown.DefaultConfig())) 按照filedown.Config自定义
	//appInstance.Engine.SetM3U8(m3u8.NewDownloader(m3u8.DefaultConfig())) 按照m3u8.Config自定义

	//=========================注册业务逻辑所需要的爬虫流程阶段=========================//
	// 参数： 传入对应的爬虫业务层fetcher, 回调方法用于初始任务手动提交
	// 阶段一: 抓取分类目录页(需要做一次手动提交将目录页url传入任务) 采集:url 标题 分类信息
	// 注:fetcher.FetchFirst{}中GetStage()返回字串需要与配置中的stage保持一致 crawler.stages.first
	appInstance.RegisterStage(&fetcher.FetchFirst{},
		func(engine *crawler.Engine) {
			// 手动提交起始任务 任务需标注下一个阶段为流水作业需要
			//一般用于初期目录阶段后续任务分发均由fetcher步骤里具体实现
			if err := engine.SubmitTask(&crawler.Task{
				PID:        0, //初级任务
				URL:        appInstance.Config.Crawler.Target + "classify?type=rexue",
				Stage:      "catalog",
				Repeatable: true, //可重复抓取，为后期轮询标识
			}); err != nil {
				appInstance.Logger.Engine.Errorf("submit initial task: %s", err.Error())
			}
		})
	// 阶段二: 抓取详情目录页内容
	// 注:fetcher.FetchSecond{} GetStage()返回字串 second 同配置 crawler.stages.second
	appInstance.RegisterStage(&fetcher.FetchSecond{}, nil)
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
