package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/auth"
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

// whitelistReloadInterval 白名单的重载间隔。
//
// 为什么要定期重载：库是唯一事实来源，而库会被**别人**改 —— 多实例后台（A 实例在设置页改完，
// B 实例要能看见）、运维直接改 SQL 修一行。本进程自己保存的那次是即时生效的（写完就换缓存），
// 这个 ticker 只兜上面两种；30s 是"够快，也不至于拿它当热路径"的量级。
const whitelistReloadInterval = 30 * time.Second

// whitelistCacheTTL 白名单读取的进程内缓存时长（装饰器 CachedStringListReader 的 TTL）。
//
// 重载循环每 30 秒跑一次，但不该每 30 秒查一次库：白名单值几乎不变，读库结果缓存 5 分钟后
// 才真正回源。代价是"别的进程/运维直接改 SQL"最迟约 5 分钟可见（本进程后台保存会 Invalidate，
// 即时生效）。要调可见性/查库频率的平衡就改这个常量。
const whitelistCacheTTL = 5 * time.Minute

// tokenCountCacheTTL 是「访问令牌数量」的进程内缓存时长（auth.TokenCounter 的 TTL）。
//
// 数量缓存仅保留管理统计用途，不能参与鉴权。Verifier/Confirmer 每次查库，Required 恒为 true。
// 本进程令牌页写操作仍使计数失效；跨进程的令牌创建、停用和删除无需等待此 TTL 即可影响下一次鉴权。
const tokenCountCacheTTL = time.Minute

// reloadWhitelistLoop 定期把库里的白名单同步进本进程的判定缓存。
//
// 读经 CachedStringListReader：30 秒的 tick 只打内存，真正的 SELECT 由 TTL 摊薄。
func (a *App) reloadWhitelistLoop(ctx context.Context, reader configstore.StringListReader, mon *server.Monitor) {
	ticker := time.NewTicker(whitelistReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mon.SetWhitelist(access.LoadWhitelist(reader, a.Logger.Sys))
		}
	}
}

// tokenWriteHook 接「访问令牌」页的写操作：成功后让令牌数量缓存失效，同时按开关记审计。
//
// 成功写入后更新管理计数；失败事件不失效（数据没变）。鉴权独立逐请求查库，不依赖此 hook。
//
// 抽成独立函数是为了能离线断言"写成功确实 Invalidate"——它的调用点在 httpServer 里，
// 那一步要真库真端口。
func tokenWriteHook(counter *auth.TokenCounter, rec *oplog.Recorder) tokenadmin.Hook {
	return func(ev tokenadmin.Event) {
		if ev.Err == nil {
			counter.Invalidate()
		}
		if rec == nil {
			return
		}
		rec.RecordEvent(oplog.Event{
			Table: ev.Table, Action: ev.Action,
			RowID:    strconv.FormatUint(uint64(ev.ID), 10),
			Values:   ev.Values,
			Err:      ev.Err,
			IP:       ev.IP,
			Operator: auth.OperatorFrom(ev.Req.Context()),
			At:       ev.At,
		})
	}
}

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

	a.UsePage(Page{Key: "browser-diagnostics", Label: "浏览器诊断", Section: "system", Script: rodDiagnosticsScript})
	a.UsePage(Page{Key: "crawl-control", Label: "采集控制", Section: "monitor", Script: runtimeControlScript})
	a.UsePage(Page{Key: "account-pools", Label: "账号池", Section: "data", Script: accountPoolsScript})
	a.UsePage(Page{Key: "accounts", Label: "账号管理", Section: "data", Script: accountsScript})
	mux := http.NewServeMux()
	if a.Config.TaskFlow.Enabled {
		a.UseCSS(server.TaskFlowCSS)
		a.UsePage(Page{Key: "task-flow-policy", Label: "运行调度策略", Group: "调度", Section: "flow", Script: server.TaskFlowScript})
	}

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
	whitelist := access.LoadWhitelist(whitelistReader, a.Logger.Sys)
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
				rec.RecordEvent(oplog.Event{Table: "crawler_task_flows", Action: ev.Action, RowID: ev.ID, Operator: ev.Operator, IP: ev.IP, At: ev.At, Err: ev.Err})
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
			rec.RecordEvent(oplog.Event{
				Table:    ev.Queue,
				Action:   "process",
				Values:   map[string]any{"processed": ev.Processed},
				Err:      ev.Err,
				IP:       ev.IP,
				Operator: ev.Operator,
				At:       ev.At,
			})
		},
		TaskTrace:          a.Engine.ListTrace,
		VerifyTokenValue:   tokenCounter.Confirmer(a.Logger.Sys),
		BreakerStatuses:    a.Engine.BreakerStatuses,
		BreakerReports:     a.Engine.BreakerReports,
		ForceResumeBreaker: a.Engine.ForceResumeSite,
		OnBreakerForceResume: func(operator, site string) {
			if rec != nil {
				rec.RecordEvent(oplog.Event{Table: "breaker", Action: "force_resume", Values: map[string]any{"scope": site, "note": "人工强制解除保护；未验证健康，保留失败计数"}, Operator: operator, At: time.Now()})
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
			// 放行是**人在场的干预**：熔断停了整条抓取线，谁在什么时候放的行必须查得到
			if rec == nil {
				return
			}
			values := map[string]any{
				"note":  "请求重试整站恢复或解除普通人工暂停；不跳过自动验证",
				"scope": site, // 哪个站（站点是必填维度，不存在空键）
			}
			if stage != "" {
				values["note"] = "手动放行被暂停的抓取（只放行这个阶段）"
				values["stage"] = stage
			}
			rec.RecordEvent(oplog.Event{
				Table:    "breaker",
				Action:   "resume",
				Values:   values,
				Operator: operator,
				At:       time.Now(),
			})
		},
		PauseBreaker: func(site, stage string) bool {
			// 暂停同样是人**在场的干预**（它停了整站的抓取线），所以 reason 由宿主给死，
			// 后台只管点：操作日志里能看清"是谁在后台按的"。
			const reason = "后台手动暂停"
			if stage == "" {
				return a.Engine.PauseSite(site, reason)
			}
			return a.Engine.PauseStage(site, stage, reason)
		},
		OnBreakerPause: func(operator, site, stage string) {
			if rec == nil {
				return
			}
			values := map[string]any{
				"note":  "后台手动暂停了这一站的抓取",
				"scope": site,
			}
			if stage != "" {
				values["note"] = "后台手动暂停了这一站的某个阶段"
				values["stage"] = stage
			}
			rec.RecordEvent(oplog.Event{
				Table:    "breaker",
				Action:   "pause",
				Values:   values,
				Operator: operator,
				At:       time.Now(),
			})
		},
	})
	mon.Register(mux)
	// 运维直接改 SQL 后的收敛：定期把库里的白名单同步进判定缓存
	go a.reloadWhitelistLoop(ctx, whitelistReader, mon)

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
	mux.Handle("/api/browser-diagnostics", mon.Auth(RodDiagnostics(a)))
	mux.Handle("/api/browser-identities", mon.Auth(http.HandlerFunc(a.browserIdentitiesAPI)))
	mux.Handle("/api/runtime-policy", mon.Auth(http.HandlerFunc(a.runtimePolicyAPI)))
	mux.Handle("/api/runtime-remote", mon.Auth(http.HandlerFunc(a.runtimeRemoteAPI)))
	mux.Handle("/api/account-pools", mon.Auth(http.HandlerFunc(a.accountPoolsAPI)))
	mux.Handle("/api/accounts", mon.Auth(http.HandlerFunc(a.accountsAPI)))
	mux.Handle("/api/accounts/remote", mon.Auth(http.HandlerFunc(a.accountsRemoteAPI)))
	mux.Handle("/api/account-compensation", mon.Auth(http.HandlerFunc(a.accountCompensationAPI)))
	mux.Handle("/api/runtime-policy/history", mon.Auth(http.HandlerFunc(a.runtimeHistoryAPI)))
	mux.Handle("/api/proxy-health", mon.Auth(http.HandlerFunc(a.proxyHealthAPI)))

	// 自定义页与注入（阶段 3 的逃生舱）
	a.mountCustomRoutes(mux, mon.Auth)
	// 业务自定义路由（UseRouter）：默认套上和上面同一道鉴权，NoAuth() 的组除外
	a.mountRouters(mux, mon.Auth)

	readHeader, read, write, idle, _ := cfg.HTTPTimeouts()
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           mux,
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

// shutdownHTTP 优雅停下 HTTP 服务：不再接新请求，等在途的跑完（最多 ShutdownTimeout）。
// 超时就强制 Close，别把关停卡死。
func (a *App) shutdownHTTP() {
	if a.httpSrv == nil {
		return
	}
	_, _, _, _, timeout := a.Config.Server.HTTPTimeouts()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := a.httpSrv.Shutdown(ctx); err != nil {
		a.Logger.Sys.Errorf("http server shutdown: %s（超时，强制关闭）", err.Error())
		_ = a.httpSrv.Close()
	}
}
