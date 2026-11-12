package server

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/queues"
	"github.com/ydtg1993/papa/v4/admin/sysinfo"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/obs/incident"
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

// Logger 简单日志接口，避免循环依赖
type Logger interface {
	Info(args ...any)
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}
