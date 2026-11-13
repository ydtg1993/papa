package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/breakers"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/admin/oplog"
	"github.com/ydtg1993/papa/v4/admin/server"
	"github.com/ydtg1993/papa/v4/admin/sysinfo"
	"github.com/ydtg1993/papa/v4/admin/tokenadmin"
	"github.com/ydtg1993/papa/v4/engine"
	"net/http"
	"strconv"
	"time"
)

// Reload observes operator edits to persistent configuration. Local saves
// invalidate the shared cache immediately; background SQL is limited by TTL.
const whitelistReloadInterval = 30 * time.Second
const whitelistCacheTTL = 5 * time.Minute

// Token count is administrative information, never an authentication cache.
const tokenCountCacheTTL = time.Minute

// httpServer 启动统一 HTTP 服务（监控后台）
func (a *App) httpServer(ctx context.Context) {
	cfg := a.Config.Server
	if !cfg.Enabled {
		// 服务没起来 = 后台的内容（表格页 / 自定义页 / 自定义路由）一个都不会挂载。
		// 不能静默：「代码写了、开关没开、什么都不报」是最难查的一类问题（同「表不存在」那条日志）。
		if len(a.routers) > 0 {
			a.Logger.Sys.Errorf("已注册 %d 组自定义路由，但 HTTP 服务未启用（server.enabled=false）—— 这些路由不会挂载", len(a.routers))
		}
		return
	}

	a.registerBuiltinPages()
	mux := http.NewServeMux()

	getter := func() map[string]engine.StageStats {
		return a.Engine.GetStageStats()
	}
	if a.sysInfo == nil {
		a.sysInfo = sysinfo.NewCollector(2*time.Second, cfg.MonitorDirs)
		a.sysInfo.Start(ctx)
	}
	// 来源白名单存在 crawler_config 表里（运行期可在设置页改），不在 config.yaml —— 配置里写旧键
	// （server.whitelist / whitelist_file）会在**配置校验**那一关就拒绝启动：未知键一律拒收。
	whitelistStore := a.configStore // 与站点快照共用同一个读写口（同一份内存缓存）
	if err := whitelistStore.EnsureTable(); err != nil {
		// 表不存在就直接起不来。少了它白名单会**静默失效**（读不到 = 不限制来源），一个字都不报 ——
		// 而"起不来"至少把问题摆在运维面前（报错里点名 `papa migrate`）。
		panic(fmt.Errorf("后台运行期配置表没就绪：%w", err))
	}
	// 读白名单的装饰器：重载循环每 30s 打一次内存，真正查库由 TTL 摊薄（见 whitelistCacheTTL）。
	whitelistReader := configstore.NewCachedStringListReader(whitelistStore, whitelistCacheTTL)
	whitelist, err := access.LoadWhitelistContext(ctx, whitelistReader, a.Logger.Sys)
	if err != nil {
		// A stop arriving during initialization is not a configuration error.
		// The Run coordinator still drains everything acquired so far.
		a.Logger.Sys.Infof("admin initialization canceled: %s", err)
		return
	}
	access.WarnIfOpenToAll(a.Logger.Sys, whitelist)
	// 访问令牌：库表里多条、每条属于一个操作人（原来配置里的单 auth_key 已废弃）
	auth.WarnIfNoToken(a.DB, a.Logger.Sys)

	// 共享令牌服务：鉴权逐请求查库，数量缓存仅用于管理统计兼容。
	tokenCounter := auth.NewTokenCounter(a.DB, tokenCountCacheTTL)

	// 操作日志：开启时，oao 表格页的动作、后台自带的「访问令牌」页、以及熔断放行都往这里记。
	// 它得**先于** MonitorConfig 构造 —— 熔断放行的 OnBreakerResume 回调要闭包住这个 rec。
	var rec *oplog.Recorder
	if cfg.OperationLog {
		rec = oplog.New(a.DB, a.Logger.Sys)
		a.oplog = rec
	}

	mon := server.NewMonitor(getter, a.Logger.Sys, server.MonitorConfig{
		OnTaskFlowAction: func(ev server.TaskFlowActionEvent) {
			if rec != nil {
				rec.RecordEvent(ev.OperationLog())
			}
		},
		TaskFlow:       a.Engine.TaskFlow,
		UpdateTaskFlow: a.Engine.UpdateTaskFlow,
		VerifyToken:    tokenCounter.Verifier(a.Logger.Sys),
		// 后台页面首帧始终要求登录；首次令牌由本机 CLI 创建。
		AuthRequired: tokenCounter.Required,
		Whitelist:    whitelist,
		// 设置页保存白名单 → 落 crawler_config；成功后 admin/server 自己换判定缓存（本进程即时生效）。
		SaveWhitelist: func(entries []string, operator string) error {
			if err := whitelistStore.Save(configstore.KeyIPWhitelist, entries, operator); err != nil {
				return err
			}
			// 写成功才失效缓存：否则 30s 后的重载会拿旧缓存把刚保存的值盖回内存。
			whitelistReader.Invalidate(configstore.KeyIPWhitelist)
			return nil
		},
		QueueStats: a.Engine.GetQueueStats,
		SiteStats:  a.Engine.GetSiteStats,
		SysInfo:    a.sysInfo,
		LogDir:     a.Config.Log.Dir,
		// obs 报告目录：「obs报告」页列这里的历史运行报告（papa report / 退出自动报告写同一目录）。
		ReportDir:          a.Config.Obs.Report.Dir,
		ArchiveDir:         a.Engine.ArchiveDir(),
		OnShutdown:         a.Shutdown,
		ProcessErrorQueue:  a.Engine.ProcessErrorQueue,
		ProcessRepeatQueue: a.Engine.RepollRepeatableTasks,
		// 队列「立即执行」也是人在场的干预（批量重置 + 重投），成败都写审计 ——
		// 与上面熔断放行同一个理由，也与表格动作/令牌页保持一致。
		OnQueueProcess: func(ev server.QueueProcessEvent) {
			if rec == nil {
				return
			}
			rec.RecordEvent(ev.OperationLog())
		},
		TaskTrace:          a.Engine.ListTrace,
		VerifyTokenValue:   tokenCounter.Confirmer(a.Logger.Sys),
		BreakerStatuses:    a.Engine.BreakerStatuses,
		BreakerReports:     a.Engine.BreakerReports,
		ForceResumeBreaker: a.Engine.ForceResumeSite,
		OnBreakerForceResume: func(operator, site string) {
			if rec != nil {
				rec.RecordEvent(breakers.ForceResumeEvent(operator, site, time.Now()))
			}
		},
		// 暂停/放行的 scope 有两层：`stage` 为空 = 整站，否则只动那一站的某个阶段。
		// 两层写的是**同一张暂停信号表**（见 engine/pause.go），只是 scope 不同。
		ResumeBreaker: func(site, stage string) bool {
			if stage == "" {
				return a.Engine.ResumeSite(site)
			}
			return a.Engine.ResumeStage(site, stage)
		},
		OnBreakerResume: func(operator, site, stage string) {
			if rec != nil {
				rec.RecordEvent(breakers.ResumeEvent(operator, site, stage, time.Now()))
			}
		},
		PauseBreaker: func(site, stage string) bool {
			// 暂停同样是人**在场的干预**（它停了整站的抓取线），所以 reason 由宿主给死，
			// 后台只管点：操作日志里能看清"是谁在后台按的"。
			const reason = breakers.ManualPauseReason
			if stage == "" {
				return a.Engine.PauseSite(site, reason)
			}
			return a.Engine.PauseStage(site, stage, reason)
		},
		OnBreakerPause: func(operator, site, stage string) {
			if rec != nil {
				rec.RecordEvent(breakers.PauseEvent(operator, site, stage, time.Now()))
			}
		},
	})
	mon.SetTaskTraceExecution(a.Engine.ListTraceExecution)
	mon.Register(mux)
	// 运维直接改 SQL 后的收敛：定期把库里的白名单同步进判定缓存
	a.whitelistReload = access.NewWhitelistReloader(whitelistReader, a.Logger.Sys, mon.SetWhitelist, whitelistReloadInterval)
	a.whitelistReload.Start(ctx)

	// Data/resources is a task operations workspace, not a system table browser.
	tables := taskDataTables(a.DB, a.Engine)
	tables = append(tables, a.tables...)
	cfgOao := oao.Config{
		Tables: tables,
		Logger: a.Logger.Sys,
		Auth:   mon.Auth, // 复用监控后台的白名单 + 令牌校验
	}
	if rec != nil {
		cfgOao.OnAction = func(ev oao.ActionEvent) { rec.Record(ev, operatorOf(ev)) }
		cfgOao.Tables = append(tables, oplog.Table(a.DB))
	}
	o, err := oao.New(cfgOao)
	if err != nil {
		a.Logger.Sys.Errorf("init table component: %s", err.Error())
	} else {
		o.Mount(mux)
		if sub, err := o.StaticFS(); err == nil {
			mux.Handle("/static/oao/", http.StripPrefix("/static/oao/",
				server.NoCache(http.FileServer(http.FS(sub)))))
		} else {
			a.Logger.Sys.Errorf("oao static fs: %s", err.Error())
		}
	}

	// 「访问令牌」页是后台自带的模块，不走表格组件 —— 它要「新增」，
	// 而表格组件的动作只回 {"status":"ok"}，没法把服务端生成的明文令牌交给操作人。
	// 写操作与表格页一样记进操作日志（用条件更新防重复点击，见 admin/tokenadmin）。
	// hook 同时使管理计数失效；令牌鉴权不依赖数量缓存。
	tokenHook := tokenWriteHook(tokenCounter, rec)
	tokenadmin.NewAPI(tokenadmin.NewStore(a.DB), tokenHook, a.Logger.Sys).Register(mux, mon.Auth)
	a.mountAdminRoutes(mux, mon.Auth)

	// 自定义页与注入（阶段 3 的逃生舱）
	a.mountCustomRoutes(mux, mon.Auth)
	// 业务自定义路由（UseRouter）：默认套上和上面同一道鉴权，NoAuth() 的组除外
	a.mountRouters(mux, mon.Auth)

	readHeader, read, write, idle, _ := cfg.HTTPTimeouts()
	a.httpRequests = newRequestDrain()
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           a.httpRequests.wrap(mux),
		ReadHeaderTimeout: readHeader,
		ReadTimeout:       read,
		WriteTimeout:      write, // 默认 0 = 不限：日志下载不能被掐
		IdleTimeout:       idle,
	}
	a.httpSrv = srv
	a.Logger.Sys.Infof("http server starting on :%d", cfg.Port)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.Logger.Sys.Errorf("http server failed: %s", err.Error())
		}
	}()
	// 这里**不再**自己关服务：关停要排在「等在途请求跑完 → 停引擎 → 关库」这条链的最前面，
	// 顺序在 Run 里统一走（见 shutdownHTTP）。原来在这里 `<-ctx.Done(); srv.Close()`
	// 会和引擎/数据库的收尾并发，把在途的日志下载直接掐断 —— 与"优雅退出"的说法不符。
}

// shutdownHTTP stops admission and waits for handlers, including hijacked
// handlers. Force-closing a connection never proves its handler has returned.
func (a *App) shutdownHTTP() bool {
	if a.httpSrv == nil && a.httpRequests == nil && a.whitelistReload == nil {
		return true
	}
	a.httpRequests.stop()
	if a.whitelistReload != nil {
		a.whitelistReload.Cancel()
	}
	_, _, _, _, timeout := a.Config.Server.HTTPTimeouts()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	drained := true
	if a.httpSrv != nil {
		if err := a.httpSrv.Shutdown(ctx); err != nil {
			a.Logger.Sys.Errorf("http server shutdown: %s（强制关闭连接，保留共享资源）", err.Error())
			_ = a.httpSrv.Close()
			drained = false
		}
	}
	if !a.httpRequests.wait(ctx) {
		drained = false
	}
	if a.whitelistReload != nil && !a.whitelistReload.Stop(ctx) {
		drained = false
	}
	return drained
}
