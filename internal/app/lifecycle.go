package app

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"sync"
	"time"
)

// Run 启动引擎，等待退出信号
func (a *App) Run(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	started := false
	// Startup failures use the same drain/close decisions as normal shutdown.
	// Cancel first even when unwinding a panic before the HTTP server is ready.
	defer func() {
		cancel()
		a.shutdownRun(started)
	}()
	if err := a.initRuntime(runCtx); err != nil {
		panic(fmt.Errorf("采集控制初始化失败: %w", err))
	}

	a.emitRunStarted()
	started = true
	// 定时报告（obs.report.interval）：与退出报告共用同一套日报产物，停机时会先停它再出最终报告。
	a.startReportLoop()

	// 初始化引擎浏览器池
	a.Engine.SetBrowserPool()
	a.BrowserPool = a.Engine.GetBrowserPool() // 同步给导出字段：外部读 App.BrowserPool 不该一直是 nil
	// 初始化静态 HTML 抓取客户端（读取请求头与代理配置）
	a.Engine.SetHTMLClient()
	// 下载器（文件 / m3u8）：框架按 DefaultConfig 备好**基座** —— 它们没有"全局那层配置面"，
	// 要按站点/阶段各配一份写在声明里。必须在 ApplyRegisterStage 之前：两层覆盖是叠在基座上的。
	a.Engine.InitDownloaders()
	if err := a.Engine.InitBreakerRuntime(runCtx); err != nil {
		panic(fmt.Errorf("initialize breaker runtime: %w", err))
	}
	// 启用工作流和对应工作池
	a.Engine.ApplyRegisterStage()

	// 监听c错误日志 中间件活动等队列消息
	a.mdMsgListener(runCtx)
	// 启动统一 HTTP 服务（监控页面）
	a.httpServer(runCtx)

	//触发结束任务 清理资源
	<-runCtx.Done()
}

func (a *App) shutdownRun(started bool) {
	a.Logger.Sys.Info("stopping application components...")
	// 顺序要紧：先把 HTTP 停下来、等在途请求（比如日志下载）跑完，再动引擎和数据库。
	// 反过来的话，正在下载日志的请求会在读到大半时被关掉的连接 / 关掉的库打断。
	httpDrained := a.shutdownHTTP()
	if !httpDrained {
		a.Logger.Sys.Error("HTTP 请求或白名单重载未排空，跳过关库与浏览器池关闭")
	}
	businessDrained := a.stopExtensions(a.Config.Crawler.StopTimeoutOrDefault())

	// 引擎排空：Engine.Stop 内部先 e.cancel()，再等各阶段 worker 把队列跑完（上限 crawler.stop_timeout）。
	// 各阶段是并发等的，所以这里最多等一个 stopTimeout。
	stopTimeout := a.Config.Crawler.StopTimeoutOrDefault()
	drained, stopStats := a.Engine.Stop(stopTimeout)
	runtimeDrained := a.drainRuntime(stopTimeout)
	if !runtimeDrained {
		a.Logger.Sys.Errorf("账号登录、账号核验或浏览器身份诊断未在 %s 内排空，跳过关库与浏览器池关闭", stopTimeout)
	}
	listenersDrained := a.stopMessageListeners(stopTimeout)
	if !listenersDrained {
		a.Logger.Sys.Errorf("错误日志监听未在 %s 内退出，跳过关库与浏览器池关闭", stopTimeout)
	}
	if manager := a.Engine.GetProxy(); manager != nil {
		manager.Close()
	}
	if !drained {
		a.Logger.Sys.Errorf("引擎未在 %s 内排空，仍有任务未跑完 %+v —— 跳过关库与浏览器池关闭，让在途写入完成；"+
			"留在库里的 pending 任务下次启动的启动恢复会重新入队", stopTimeout, stopStats)
	}

	// 事件流：先写 run_stopped，再排空 sink。必须早于关库 —— 报告 on_exit 还要读库。
	//
	// 定时报告先停：否则它可能和退出报告并发写同一天目录，或在关库之后还在读库。
	if a.reportRunner != nil {
		a.stopReportLoop()
	}
	if started {
		a.emitRunStopped(drained, stopStats)
	}
	a.closeEvents()

	// 进程退出自动出报告（obs.report.on_exit）：在关库之前生成，读到的是完整事件与权威状态。
	if started {
		a.generateExitReport()
	}

	// 审计队列先排空再关库：反过来的话最后几条（包括"优雅退出"这条操作本身）写不进去，
	// 只能落到日志文件里。写失败不阻塞响应是常态，但关停这一下要给它一个收尾的机会。
	if a.oplog != nil {
		a.oplog.Close(3 * time.Second)
	}

	// 没排空时**不关**浏览器池和数据库：pool.Stop 超时后返回，但 Go 杀不掉 goroutine，
	// worker 还在跑 —— 它们可能正持着浏览器、正要把结果写回 DB。这会儿关掉，
	// 等于把「让在途任务跑完」这件事又亲手掐断，而且剩下的每次 任务结算 / SaveResult
	// 都会报 "sql: database is closed"（日志刷屏，结果照样丢）。
	// 关库/关浏览器本是礼仪不是必须：进程退出时由 OS 回收连接，Chrome 由 leakless 收掉。
	if httpDrained && drained && businessDrained && runtimeDrained && listenersDrained {
		if a.Engine.GetBrowserPool() != nil {
			a.Engine.GetBrowserPool().Close()
		}
		if a.DB != nil {
			if sqlDB, err := a.DB.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
	}
	a.Logger.Sys.Info("shutdown completed")
}

// waitAppLoop waits for one owned background loop without extending the
// caller's deadline. A nil channel means the loop was never started.
func waitAppLoop(ctx context.Context, done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// Shutdown 触发优雅退出（供监控后台等外部调用）
func (a *App) Shutdown() {
	if a.cancel != nil {
		a.cancel()
	}
}

// mdMsgListener 监听中间件活动日志和错误
func (a *App) mdMsgListener(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	a.messageListeners = messageListenerState{cancel: cancel, done: done}
	var wg sync.WaitGroup
	listenErrors := func(errors <-chan error, logError func(error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case err, ok := <-errors:
					if !ok {
						return
					}
					if err != nil {
						logError(err)
					}
				}
			}
		}()
	}
	//监听引擎各stage的workerpool池消息
	for _, e := range a.Engine.Errors() {
		listenErrors(e, func(err error) { a.Logger.Engine.Errorf("engine error: %s", err.Error()) })
	}
	//监听中间件消息
	listen := func(md middleware.Err, log *logrus.Logger) {
		listenErrors(md.GetErrors(), func(err error) { log.Error(err) })
	}
	if proxy := a.Engine.GetProxy(); proxy != nil {
		listen(proxy, a.Logger.Proxy)
	}
	if filedown := a.Engine.GetFiledown(); filedown != nil {
		listen(filedown, a.Logger.Filedown)
	}
	if m3u8 := a.Engine.GetM3U8(); m3u8 != nil {
		listen(m3u8, a.Logger.M3U8)
	}
	go func() { wg.Wait(); close(done) }()
}

type messageListenerState struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *App) stopMessageListeners(timeout time.Duration) bool {
	if a.messageListeners.cancel != nil {
		a.messageListeners.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return waitAppLoop(ctx, a.messageListeners.done)
}
