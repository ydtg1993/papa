package server

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/breakers"
	"github.com/ydtg1993/papa/v4/admin/logs"
	"github.com/ydtg1993/papa/v4/admin/monitoring"
	"github.com/ydtg1993/papa/v4/admin/queues"
	"github.com/ydtg1993/papa/v4/admin/reports"
	"github.com/ydtg1993/papa/v4/admin/settings"
	"github.com/ydtg1993/papa/v4/admin/shell"
	"github.com/ydtg1993/papa/v4/admin/sysinfo"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
	"github.com/ydtg1993/papa/v4/admin/tasktrace"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"io/fs"
	"net"
	"net/http"
)

// MonitorGetter 定义获取所有阶段统计快照的函数类型
type MonitorGetter func() map[string]core.StageStats

// QueueProcessEvent 一次队列「立即执行」的结局（宿主拿它记操作日志）。
//
// 为什么要有它：这两个端点与「熔断放行」同一处境 —— 都是后台自带的原生接口、不走表格组件，
// 而它们同样会**批量改数据**（把到点的可轮询任务/失败任务重置并重投）。审计不该因为
// "这个按钮不是表格动作"就断档。
type QueueProcessEvent = queues.QueueProcessEvent

// TaskFlowActionEvent 一次任务流策略动作的结局，宿主拿它记操作日志。
type TaskFlowActionEvent = taskflow.TaskFlowActionEvent

// MonitorConfig 监控服务配置
type MonitorConfig struct {
	OnTaskFlowAction func(TaskFlowActionEvent)
	TaskFlow         func(context.Context, string) (engine.TaskFlowView, error)
	UpdateTaskFlow   func(context.Context, string, uint64, engine.FlowPolicy) error
	// VerifyToken 校验访问令牌，返回（操作人, 是否通过）；为 nil 时拒绝访问。
	// 由宿主注入（papa 用 admin/auth 的库表实现），本包不认识令牌怎么存 ——
	// 这样这段安全关键逻辑可以用桩离线测，包也不必依赖 gorm。
	VerifyToken func(r *http.Request) (operator string, ok bool)
	// Whitelist 初始来源 IP/CIDR 白名单（宿主从**配置存储**读来的第一份）；空=不限制。
	// 后续的变化（设置页保存 / 宿主的定期重载）经 SetWhitelist 进本包，不经这里。
	Whitelist []string
	// SaveWhitelist 把白名单**持久化**（宿主注入：papa 落在 crawler_config 表的 ip_whitelist 行）。
	// nil = 这个宿主没接配置存储 —— 保存接口返回 404 说明原因，而不是"只改内存、重启就没了"。
	SaveWhitelist func(entries []string, operator string) error
	QueueStats    func() map[string]core.QueueStat // 治理队列运行快照（可空）
	SiteStats     func() map[string]core.SiteStat  // 站点维度快照（站点信息 + 慢变统计，可空）
	SysInfo       *sysinfo.Collector               // 系统指标采集器（可空）
	LogDir        string                           // 日志目录（导出用）
	// ReportDir obs 报告根目录（`obs.report.dir`；可空 = 没开报告）。后台「obs报告」页据此列出
	// 历史运行目录并读取四份产物（report.md / report.json / ai-brief.md / ai-brief.json）。
	ReportDir string
	// ArchiveDir 页面归档根目录（可空 = 归档没开）。后台「追踪」抽屉里那条"归档页面"步骤
	// 据此提供**下载**：`GET /api/task/page?file=<相对路径>`（原始 HTML，拿去本地分析）。
	ArchiveDir string
	OnShutdown func() // 优雅退出回调
	// ProcessErrorQueue 错误队列手动触发回调（可空）：全局一条，无站点参数。
	ProcessErrorQueue func() (int, error)
	// ProcessRepeatQueue 周期轮询队列手动触发回调（可空）：全局一条，无站点参数。
	ProcessRepeatQueue func() (int, error)
	// OnQueueProcess 队列「立即执行」跑完（成功或失败都算）后的回调，宿主拿它记操作日志；可空。
	// 失败也要记：站点名打错这种"没跑成"的触发，同样是要查的线索。
	OnQueueProcess func(QueueProcessEvent)
	TaskTrace      func(id int) ([]core.TraceStep, error) // 单任务步骤追踪（可空）
	// BreakerStatuses 各站点的熔断状态快照：key 是站点 Key（站点是必填维度，没有空键那一档）。
	// 可空；也随 /api/monitor 一起返回（`breakers` 数组）。
	BreakerStatuses func() map[string]core.BreakerStatus
	// BreakerReports loads durable lifecycle reports, including final reports.
	// It is the sole report source; no process-local report fallback.
	BreakerReports func(context.Context, string) ([]incident.Lifecycle, error)
	// ResumeBreaker 重试站点的自动恢复，或解除纯人工暂停；stage 非空只解除该阶段人工暂停。
	// 返回是否接受了重试或解除了人工暂停；不代表已验证恢复。
	// 要知道有哪些站点，看 BreakerStatuses 的键。
	ResumeBreaker func(site, stage string) bool
	// ForceResumeBreaker explicitly bypasses whole-site verification.
	ForceResumeBreaker   func(site string) bool
	OnBreakerForceResume func(operator, site string)
	// OnBreakerResume 放行成功后的回调，宿主拿它记操作日志（人在场的干预必须留痕）；可空。
	// `stage` 为空 = 整站那一层。
	OnBreakerResume func(operator, site, stage string)
	// PauseBreaker 暂停某个站点（`stage` 为空）或**某站的某个阶段**的抓取（写一条暂停信号）；
	// 返回是否真的从运行态切到了暂停。与 ResumeBreaker 是一对：
	// 后台卡片上那个站点级的「暂停/恢复」，以及每个阶段行上的那个。
	PauseBreaker func(site, stage string) bool
	// OnBreakerPause 暂停成功后的回调。**与 OnBreakerResume 一样只带操作人、站点与阶段**（不带 IP）——
	// 这一对是对称的，要补 IP 就两边一起补。
	OnBreakerPause func(operator, site, stage string)
	// VerifyTokenValue 单独校验一个**令牌值**（不是请求头），给关停这类高危操作做二次确认用：
	// 调用方要把自己的令牌放进请求体再输一遍。nil 表示不支持关停（该接口直接 404）。
	VerifyTokenValue func(token string) (operator string, ok bool)
	// AuthRequired 仅控制登录遮罩的首帧显示；nil 默认要求登录。
	// 自定义身份提供方可调整页面展示，但此回调不改变接口的 VerifyToken 校验。
	AuthRequired func() bool
}

// Monitor 监控/后台管理 HTTP 路由(不负责 server 生命周期,统一由 App 层挂载)
type Monitor struct {
	page       *shell.Renderer
	access     *access.Handler
	settings   *settings.Handler
	monitoring *monitoring.Handler
	queues     *queues.Handler
	breakers   *breakers.Handler
	taskflow   *taskflow.Handler
	logs       *logs.Handler
	reports    *reports.Handler
	tasktrace  *tasktrace.Handler
}

// Logger 简单日志接口，避免循环依赖
type Logger interface {
	Info(args ...any)
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// NewMonitor 创建监控路由
func NewMonitor(getter MonitorGetter, logger Logger, cfg MonitorConfig) *Monitor {
	s := &Monitor{}
	s.access = access.New(access.Options{VerifyToken: cfg.VerifyToken, AuthRequired: cfg.AuthRequired, SaveWhitelist: cfg.SaveWhitelist, Whitelist: cfg.Whitelist}, logger)
	s.page = shell.NewRenderer(templateFS, shell.RenderOptions{AuthRequired: s.access.AuthRequired, OptionalAssets: optionalOaoAssets, Logger: logger})
	s.settings = settings.New(settings.Options{Whitelist: s.access.WhitelistSnapshot, WhitelistPersistent: cfg.SaveWhitelist != nil, LogDir: cfg.LogDir, OnShutdown: cfg.OnShutdown, VerifyTokenValue: cfg.VerifyTokenValue}, logger)
	s.monitoring = monitoring.New(monitoring.Options{Stages: getter, QueueStats: cfg.QueueStats, BreakerStatuses: cfg.BreakerStatuses, SiteStats: cfg.SiteStats, SysInfo: cfg.SysInfo})
	s.queues = queues.New(queues.Options{ProcessErrorQueue: cfg.ProcessErrorQueue, ProcessRepeatQueue: cfg.ProcessRepeatQueue, OnQueueProcess: cfg.OnQueueProcess}, logger)
	s.breakers = breakers.New(breakers.Options{BreakerStatuses: cfg.BreakerStatuses, BreakerReports: cfg.BreakerReports, ResumeBreaker: cfg.ResumeBreaker, ForceResumeBreaker: cfg.ForceResumeBreaker, PauseBreaker: cfg.PauseBreaker, OnBreakerResume: cfg.OnBreakerResume, OnBreakerForceResume: cfg.OnBreakerForceResume, OnBreakerPause: cfg.OnBreakerPause}, logger)
	s.taskflow = taskflow.New(taskflow.Options{TaskFlow: cfg.TaskFlow, UpdateTaskFlow: cfg.UpdateTaskFlow, OnTaskFlowAction: cfg.OnTaskFlowAction})
	s.logs = logs.New(logs.Options{Dir: cfg.LogDir})
	s.reports = reports.New(reports.Options{Dir: cfg.ReportDir})
	s.tasktrace = tasktrace.New(tasktrace.Options{ArchiveDir: cfg.ArchiveDir, Trace: cfg.TaskTrace, Logger: logger})
	return s
}

// parseWhitelist 解析白名单为 CIDR 列表，单 IP 视为 /32（IPv6 视为 /128）
func parseWhitelist(items []string, logger Logger) []*net.IPNet {
	return access.ParseWhitelist(items, logger)
}

// SetWhitelist 换一份白名单（宿主在启动读取与定期重载时调；解析在本包内做）。
//
// 传进来的就是**存储里的原样字符串**：解析后的 CIDR 用于每请求判定，原样那串回显给设置页。
// 解析规则与初始值同一条路（裸 IP 补 /32、IPv6 补 /128；坏条目丢掉并记 Error）。
func (s *Monitor) SetWhitelist(entries []string) { s.access.SetWhitelist(entries) }

// Register 将监控路由注册到统一 mux 上
func (s *Monitor) Register(mux *http.ServeMux) {
	// 静态资源（CSS/JS），无需鉴权
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", noCache(http.FileServer(http.FS(sub)))))
	}

	mux.HandleFunc("/monitor", s.wrap(s.htmlHandler))
	mux.HandleFunc("/api/monitor", s.wrap(s.apiHandler))
	mux.HandleFunc("/api/task-flow", s.wrap(s.taskFlowHandler))
	mux.HandleFunc("/api/settings", s.wrap(s.settingsHandler))
	mux.HandleFunc("/api/settings/whitelist", s.wrap(s.whitelistHandler))
	mux.HandleFunc("/api/settings/shutdown", s.wrap(s.shutdownHandler))
	mux.HandleFunc("/api/errorqueue/process", s.wrap(s.errorQueueProcessHandler))
	mux.HandleFunc("/api/repeatqueue/process", s.wrap(s.repeatQueueProcessHandler))
	mux.HandleFunc("/api/breaker", s.wrap(s.breakerHandler))
	mux.HandleFunc("/api/breaker/analysis", s.wrap(s.breakerAnalysisHandler))
	mux.HandleFunc("/api/breaker/resume", s.wrap(s.breakerResumeHandler))
	mux.HandleFunc("/api/breaker/force-resume", s.wrap(s.breakerForceResumeHandler))
	mux.HandleFunc("/api/breaker/pause", s.wrap(s.breakerPauseHandler))
	mux.HandleFunc("/api/task/trace", s.wrap(s.taskTraceHandler))
	mux.HandleFunc("/api/logs", s.wrap(s.logsListHandler))
	mux.HandleFunc("/api/logs/download", s.wrap(s.logsDownloadHandler))
	mux.HandleFunc("/api/logs/tail", s.wrap(s.logsTailHandler))
	mux.HandleFunc("/api/task/page", s.wrap(s.taskPageHandler))
	// obs 报告页：列历史报告 + 读单份产物原文（download=1 下载）。目录来自 obs.report.dir。
	mux.HandleFunc("/api/obs/reports", s.wrap(s.obsReportListHandler))
	mux.HandleFunc("/api/obs/report", s.wrap(s.obsReportFileHandler))
}

// wrap 包装处理器：先 IP 白名单，再令牌校验（**仅 /api/ 数据接口**）。
// 校验通过时把「操作人」放进请求上下文，操作日志据此记人。
//
// `/api/` 前缀这条豁免是给**后台自己的页面**留的：/monitor 的 HTML 壳要先能加载出来，
// 而令牌是在那张页面里填的（填之前也得拿得到页面）。挂在同一个 mux 上的外部组件与业务路由
// 不走这里，它们走 Auth —— 那道**不看前缀**，见下面那条注释。
func (s *Monitor) wrap(next http.HandlerFunc) http.HandlerFunc { return s.access.Wrap(next) }

// Auth 把同一套「白名单 + 令牌」校验暴露成 http.Handler 中间件，
// 供挂在同一 mux 上的外部组件（如 oao 表格）与业务路由复用，避免业务侧接口绕过鉴权。
//
// **与 wrap 的差别只有一处：令牌校验对**所有路径**生效，不看 /api/ 前缀。**
// 那个前缀豁免是给后台自己的页面留的（理由见 wrap），业务路由没有这个需要 ——
// 它们要么带着令牌，要么用 NoAuth() 显式公开（Router 上那两条正是这么约定的）。
//
// 这里以前直接复用 wrap，于是**挂在非 /api/ 路径上的业务路由只剩下白名单那道门**：
// 白名单是"哪些来源可以进来"，令牌是"谁"，后者缺了就等于对白名单内的任何人开放。
func (s *Monitor) Auth(next http.Handler) http.Handler { return s.access.Auth(next) }

// ipAllowed 校验来源 IP 是否在白名单内；白名单为空则放行
func (s *Monitor) ipAllowed(r *http.Request) bool { return s.access.IPAllowed(r) }

// clientIP 取直连来源 IP（r.RemoteAddr 去掉端口）。
// 刻意不信任 X-Forwarded-For / X-Real-IP：这些头可被客户端伪造，用于白名单会留下绕过漏洞。
func clientIP(r *http.Request) net.IP { return access.ClientIP(r) }

// authOK 校验访问令牌，返回（操作人, 是否通过）。
// 未注入校验器时拒绝；缺少依赖不能变成匿名管理入口。
func (s *Monitor) authOK(r *http.Request) (operator string, ok bool) { return s.access.AuthOK(r) }

// authRequired 报告后台页面是否应该要求登录（只影响 /monitor 首帧要不要画登录遮罩，
// 不参与接口鉴权 —— 接口鉴权由 VerifyToken 决定）。nil 注入时默认要求登录。
func (s *Monitor) authRequired() bool { return s.access.AuthRequired() }

// loadTemplate 加载 HTML 模板（懒加载，线程安全）

// apiHandler 返回 JSON 格式的监控数据
func (s *Monitor) apiHandler(w http.ResponseWriter, r *http.Request) { s.monitoring.Snapshot(w, r) }

// settingsHandler 返回当前后台设置状态（不含密钥明文）
func (s *Monitor) settingsHandler(w http.ResponseWriter, r *http.Request) { s.settings.Settings(w, r) }

// maxAdminBody 后台几个收 JSON 的接口的请求体上限。
//
// 它们的 body 都只是几个短字段（白名单数组 / 运行期覆盖层），但**必须有个头**：
// `json.Decoder` 会把一个没写完的 JSON（比如一个永远不闭合的数组）一路读下去，
// 不设限就是任人喂内存 —— 这条路径还在鉴权后面，但那是"要令牌"，不是"不用管"。
// 64KB 约合 3000 条 IP/CIDR，给足余量。
const maxAdminBody = 64 << 10

// whitelistSource 白名单的持久化位置说明（回给设置页的提示文案用）。
const whitelistSource = "crawler_config.ip_whitelist"

// whitelistHandler 动态更新 IP/CIDR 白名单：交给宿主持久化（papa 落在 crawler_config 表），
// 成功后立刻换本进程的判定缓存 —— 存储是唯一事实来源，这里不自己记一份"待保存"的差异。
func (s *Monitor) whitelistHandler(w http.ResponseWriter, r *http.Request) { s.access.Whitelist(w, r) }

// shutdownHandler 优雅退出。
//
// 这是**高危操作**：要求调用方在请求体里把自己的访问令牌再输一遍（`{"token":"..."}`）。
// 中间件那道校验只证明"这个浏览器带着有效凭据"，这里要的是"人在场"——
// 误点、开着后台页面走开都不至于把服务停掉。（它不构成新的安全边界：令牌本来就是同一个，
// 作用是确认动作 + 记下操作人。）
func (s *Monitor) shutdownHandler(w http.ResponseWriter, r *http.Request) { s.settings.Shutdown(w, r) }

// auditQueueProcess 把一次「立即执行」的结局交给宿主记审计。
//
// 只记**真的跑过**的那种：接口没配置（404）属于"压根没这条功能"，不是一次触发，不记。
// 操作人与 IP 在这里取（与白名单同一套取法），宿主只负责落到 oplog。

// errorQueueProcessHandler 手动触发失败任务错误队列处理
func (s *Monitor) errorQueueProcessHandler(w http.ResponseWriter, r *http.Request) {
	s.queues.Error(w, r)
}

// repeatQueueProcessHandler 手动触发周期轮询队列处理
func (s *Monitor) repeatQueueProcessHandler(w http.ResponseWriter, r *http.Request) {
	s.queues.Repeat(w, r)
}

// sortedBreakerStatuses 把各站点的状态排成稳定顺序（按站点名字典序）。
func sortedBreakerStatuses(all map[string]core.BreakerStatus) []core.BreakerStatus {
	return breakers.SortedStatuses(all)
}

// breakerHandler 读各 scope 的熔断状态。写操作见 breakerResumeHandler。
func (s *Monitor) breakerHandler(w http.ResponseWriter, r *http.Request) { s.breakers.Status(w, r) }

// breakerResumeHandler requests bounded whole-site verification or releases a
// pure manual pause. The force endpoint is the explicit verification bypass.
func (s *Monitor) breakerResumeHandler(w http.ResponseWriter, r *http.Request) {
	s.breakers.Resume(w, r)
}

func (s *Monitor) breakerForceResumeHandler(w http.ResponseWriter, r *http.Request) {
	s.breakers.ForceResume(w, r)
}

// breakerPauseHandler 手动暂停某个站点的抓取 —— 与 resume 是一对，但**scope 必填**。
//
// 为什么不点名就 400（而 resume 不点名 = 全部放行）：放行是往安全方向走，暂停是往危险方向走 ——
// 一个误点不该把**所有**站点一起停掉。逐站点名也让操作日志能落实到具体那一站。
//
// 暂停的语义（与熔断触发时同一套）：两层一起生效 ——
//   - **取任务点**（闸门）：worker 停在取任务**之前**，队列里已经躺着的那批也冻住，在途任务跑完为止；
//   - **入队点**（收口）：不再接新活，任务寄放到溢出表，点「恢复」时立刻灌回。
//
// 所以它天然适合"多站分批放开"：停得干净、不堆积、放开即续跑。
func (s *Monitor) breakerPauseHandler(w http.ResponseWriter, r *http.Request) { s.breakers.Pause(w, r) }

// breakerTargetParam 取暂停/放行的对象：先看 `?scope=&stage=`，再看请求体 `{"scope":"…","stage":"…"}`。
//
// `scope` 是**站点键**（沿用这个端点的老叫法）；空串在 resume 里表示"全部站点"、在 pause 里是错误。
// `stage` 为空 = 整站那一层。**给了 stage 却没给 scope 一律不合法**（阶段属于某个站）——
// 调用方回 400，别静默当成"整站"或"全部"。
func breakerTargetParam(r *http.Request) (site, stage string, ok bool) {
	return breakers.TargetParam(r)
}

// taskTraceHandler 返回一条任务的步骤追踪时间线（按尝试、步骤排序）。
func (s *Monitor) taskTraceHandler(w http.ResponseWriter, r *http.Request) { s.tasktrace.Trace(w, r) }

// logFile 日志文件条目

// taskPageHandler 下载一个归档页面（`GET /api/task/page?file=<相对路径>`）。
//
// 给的是**文件本身**（`Content-Disposition: attachment`）而不是内联渲染 —— 拿下去本地分析
// （对着真实页面写选择器）比在浏览器里看更实用，也省掉"注入 base 标签才能长回原样"那套。
func (s *Monitor) taskPageHandler(w http.ResponseWriter, r *http.Request) { s.tasktrace.Page(w, r) }

// resolveArchivedFile 把一个**相对路径**解析到归档根目录下的绝对路径；越界一律拒绝。
//
// 两道判据：先按路径语义清洗（拒绝绝对路径与 `..`），再确认解析结果确实落在根之下
// （防大小写/分隔符之类的意外）。归档目录里的文件名由框架生成，但路径是**从请求里来的** ——
// 那是不可信输入，这里不靠"文件名不含 .."这种假设。
func resolveArchivedFile(root, rel string) (string, error) {
	return tasktrace.ResolveArchivedFile(root, rel)
}

// logsListHandler 列出日志目录下的文件
func (s *Monitor) logsListHandler(w http.ResponseWriter, r *http.Request) { s.logs.List(w, r) }

// logsDownloadHandler 下载单个日志文件（?file=name），缺省打包全部为 zip
func (s *Monitor) logsDownloadHandler(w http.ResponseWriter, r *http.Request) { s.logs.Download(w, r) }

// zipLogs 将日志目录下所有文件打包为 zip 响应

// noCache 禁止静态资源被浏览器缓存，改样式后无需手动清缓存即可看到。
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// NoCache 导出 noCache，供宿主挂载其它静态资源（如 oao 组件）时复用。
func NoCache(next http.Handler) http.Handler { return noCache(next) }

// htmlHandler 返回 HTML 监控页面
func (s *Monitor) htmlHandler(w http.ResponseWriter, r *http.Request) { s.page.ServeHTTP(w, r) }
