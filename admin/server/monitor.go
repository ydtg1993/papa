package server

import (
	"archive/zip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/admin/sysinfo"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
)

//go:embed template.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// MonitorGetter 定义获取所有阶段统计快照的函数类型
type MonitorGetter func() map[string]core.StageStats

// QueueProcessEvent 一次队列「立即执行」的结局（宿主拿它记操作日志）。
//
// 为什么要有它：这两个端点与「熔断放行」同一处境 —— 都是后台自带的原生接口、不走表格组件，
// 而它们同样会**批量改数据**（把到点的可轮询任务/失败任务重置并重投）。审计不该因为
// "这个按钮不是表格动作"就断档。
type QueueProcessEvent struct {
	Queue     string // 队列名：engine.QueueRepeat / engine.QueueError（与后台那一行的名字一致）
	Operator  string // 谁点的（鉴权中间件解析令牌得到；未鉴权/测试构造的请求为空）
	IP        string // 直连来源 IP（与白名单同一取法，不信 X-Forwarded-For）
	Processed int    // 实际处理条数
	Err       error  // nil = 成功
	At        time.Time
}

// MonitorConfig 监控服务配置
type MonitorConfig struct {
	// VerifyToken 校验访问令牌，返回（操作人, 是否通过）；为 nil 表示不校验（未配置凭据）。
	// 由宿主注入（papa 用 admin/auth 的库表实现），本包不认识令牌怎么存 ——
	// 这样这段安全关键逻辑可以用桩离线测，包也不必依赖 gorm。
	VerifyToken func(r *http.Request) (operator string, ok bool)
	// Whitelist 初始来源 IP/CIDR 白名单（宿主从**配置存储**读来的第一份）；空=不限制。
	// 后续的变化（设置页保存 / 宿主的定期重载）经 SetWhitelist 进本包，不经这里。
	Whitelist []string
	// SaveWhitelist 把白名单**持久化**（宿主注入：papa 落在 crawler_config 表的 ip_whitelist 行）。
	// nil = 这个宿主没接配置存储 —— 保存接口返回 404 说明原因，而不是"只改内存、重启就没了"。
	SaveWhitelist func(entries []string, operator string) error
	Metrics       func() map[string]any            // 业务自定义数据快照（可空）
	QueueStats    func() map[string]core.QueueStat // 治理队列运行快照（可空）
	SiteStats     func() map[string]core.SiteStat  // 站点维度快照（站点信息 + 慢变统计，可空）
	SysInfo       *sysinfo.Collector               // 系统指标采集器（可空）
	LogDir        string                           // 日志目录（导出用）
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
	// ResumeBreaker 放行某个站点（`stage` 为空）或**某站的某个阶段**的抓取（清掉对应的暂停信号）；
	// 返回是否真的从暂停态切了回来（本来在跑就是 false）。
	// 要知道有哪些站点，看 BreakerStatuses 的键。
	ResumeBreaker func(site, stage string) bool
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
}

// Monitor 监控/后台管理 HTTP 路由(不负责 server 生命周期,统一由 App 层挂载)
type Monitor struct {
	getter    MonitorGetter
	logger    Logger
	cfg       MonitorConfig
	template  *template.Template
	parseOnce sync.Once
	parseErr  error

	mu sync.RWMutex
	// whitelist 解析后的白名单（每请求判定用它）；rawWhitelist 是**原样**的那一份（设置页回显用它，
	// 免得把运营输入的 "192.168.1.1" 显示成规范化后的 "192.168.1.1/32"）。两者永远一起换。
	whitelist    []*net.IPNet
	rawWhitelist []string
}

// Logger 简单日志接口，避免循环依赖
type Logger interface {
	Info(args ...any)
	Infof(format string, args ...any)
	Errorf(format string, args ...any)
}

// NewMonitor 创建监控路由
func NewMonitor(getter MonitorGetter, logger Logger, cfg MonitorConfig) *Monitor {
	return &Monitor{
		getter:       getter,
		logger:       logger,
		cfg:          cfg,
		whitelist:    parseWhitelist(cfg.Whitelist, logger),
		rawWhitelist: nonEmptyStrings(cfg.Whitelist),
	}
}

// parseWhitelist 解析白名单为 CIDR 列表，单 IP 视为 /32（IPv6 视为 /128）
func parseWhitelist(items []string, logger Logger) []*net.IPNet {
	var out []*net.IPNet
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		cidr := item
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		if _, ipNet, err := net.ParseCIDR(cidr); err == nil {
			out = append(out, ipNet)
		} else {
			logger.Errorf("invalid whitelist entry %q: %s", item, err.Error())
		}
	}
	// 非空、却一条都没解析出来 —— 这一档的语义是"按不限制来源生效"（见 configstore.KeyIPWhitelist），
	// 也就是把限制**悄悄放宽了**。运营写错一个字符就会走到这里，所以按 Error 单独喊一句。
	if len(out) == 0 && len(items) > 0 {
		logger.Errorf("来源 IP 白名单配置了 %d 条、但一条都解析不出来 —— 按「不限制来源」生效。"+
			"要么改对（后台设置页 / crawler_config 表的 ip_whitelist 行），要么显式清空成 []", len(items))
	}
	return out
}

// nonEmptyStrings 去掉空串与首尾空白，保留原样的顺序（设置页回显用）。
func nonEmptyStrings(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// SetWhitelist 换一份白名单（宿主在启动读取与定期重载时调；解析在本包内做）。
//
// 传进来的就是**存储里的原样字符串**：解析后的 CIDR 用于每请求判定，原样那串回显给设置页。
// 解析规则与初始值同一条路（裸 IP 补 /32、IPv6 补 /128；坏条目丢掉并记 Error）。
func (s *Monitor) SetWhitelist(entries []string) {
	parsed := parseWhitelist(entries, s.logger)
	raw := nonEmptyStrings(entries)
	s.mu.Lock()
	s.whitelist = parsed
	s.rawWhitelist = raw
	s.mu.Unlock()
}

// firstInvalidWhitelistEntry 严格校验一份**待保存**的白名单：返回第一条不是 IP/CIDR 的条目
// （都合法、或整份为空，返回空串）。
//
// 与读路径（parseWhitelist）的差别是时机与后果：读路径跑在"库里已存的值"上，坏条目丢掉、
// 丢完为空就是不限制（那是运营定的语义）；保存路径是人在后台手输的，写坏了没有第二次机会 ——
// 所以当场拒收，把是哪一条说给操作人听。空数组允许：语义是"清空限制"。
func firstInvalidWhitelistEntry(items []string) string {
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		cidr := item
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return item
		}
	}
	return ""
}

// Register 将监控路由注册到统一 mux 上
func (s *Monitor) Register(mux *http.ServeMux) {
	// 静态资源（CSS/JS），无需鉴权
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", noCache(http.FileServer(http.FS(sub)))))
	}

	mux.HandleFunc("/monitor", s.wrap(s.htmlHandler))
	mux.HandleFunc("/api/monitor", s.wrap(s.apiHandler))
	mux.HandleFunc("/api/settings", s.wrap(s.settingsHandler))
	mux.HandleFunc("/api/settings/whitelist", s.wrap(s.whitelistHandler))
	mux.HandleFunc("/api/settings/shutdown", s.wrap(s.shutdownHandler))
	mux.HandleFunc("/api/errorqueue/process", s.wrap(s.errorQueueProcessHandler))
	mux.HandleFunc("/api/repeatqueue/process", s.wrap(s.repeatQueueProcessHandler))
	mux.HandleFunc("/api/breaker", s.wrap(s.breakerHandler))
	mux.HandleFunc("/api/breaker/resume", s.wrap(s.breakerResumeHandler))
	mux.HandleFunc("/api/breaker/pause", s.wrap(s.breakerPauseHandler))
	mux.HandleFunc("/api/task/trace", s.wrap(s.taskTraceHandler))
	mux.HandleFunc("/api/logs", s.wrap(s.logsListHandler))
	mux.HandleFunc("/api/logs/download", s.wrap(s.logsDownloadHandler))
	mux.HandleFunc("/api/task/page", s.wrap(s.taskPageHandler))
}

// wrap 包装处理器：先 IP 白名单，再令牌校验（**仅 /api/ 数据接口**）。
// 校验通过时把「操作人」放进请求上下文，操作日志据此记人。
//
// `/api/` 前缀这条豁免是给**后台自己的页面**留的：/monitor 的 HTML 壳要先能加载出来，
// 而令牌是在那张页面里填的（填之前也得拿得到页面）。挂在同一个 mux 上的外部组件与业务路由
// 不走这里，它们走 Auth —— 那道**不看前缀**，见下面那条注释。
func (s *Monitor) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ipAllowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			r2, ok := s.requireToken(w, r)
			if !ok {
				return
			}
			r = r2
		}
		next(w, r)
	}
}

// requireToken 校验令牌并把操作人放进请求上下文；不通过时**已经写好了响应**
// （401 + WWW-Authenticate），调用方直接 return 即可。
//
// 顺手打上 Cache-Control: no-store —— 带凭据的数据响应不许任何缓存留存：令牌被停用/删除后，
// 代理（或浏览器）里那份旧 200 还能被原样重放出来。浏览器自己不会缓存带 Authorization 的响应，
// 但中间代理会，所以把它明确掉，不靠实现细节。
func (s *Monitor) requireToken(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	w.Header().Set("Cache-Control", "no-store")
	operator, ok := s.authOK(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return r, false
	}
	return r.WithContext(auth.WithOperator(r.Context(), operator)), true
}

// Auth 把同一套「白名单 + 令牌」校验暴露成 http.Handler 中间件，
// 供挂在同一 mux 上的外部组件（如 oao 表格）与业务路由复用，避免业务侧接口绕过鉴权。
//
// **与 wrap 的差别只有一处：令牌校验对**所有路径**生效，不看 /api/ 前缀。**
// 那个前缀豁免是给后台自己的页面留的（理由见 wrap），业务路由没有这个需要 ——
// 它们要么带着令牌，要么用 NoAuth() 显式公开（Router 上那两条正是这么约定的）。
//
// 这里以前直接复用 wrap，于是**挂在非 /api/ 路径上的业务路由只剩下白名单那道门**：
// 白名单是"哪些来源可以进来"，令牌是"谁"，后者缺了就等于对白名单内的任何人开放。
func (s *Monitor) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.ipAllowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		r2, ok := s.requireToken(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r2)
	})
}

// ipAllowed 校验来源 IP 是否在白名单内；白名单为空则放行
func (s *Monitor) ipAllowed(r *http.Request) bool {
	s.mu.RLock()
	wl := s.whitelist
	s.mu.RUnlock()
	if len(wl) == 0 {
		return true
	}
	ip := clientIP(r)
	for _, n := range wl {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP 取直连来源 IP（r.RemoteAddr 去掉端口）。
// 刻意不信任 X-Forwarded-For / X-Real-IP：这些头可被客户端伪造，用于白名单会留下绕过漏洞。
func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// authOK 校验访问令牌，返回（操作人, 是否通过）。
// 没注入校验器（未配置凭据）时放行 —— 与"没配任何凭据"同义。
func (s *Monitor) authOK(r *http.Request) (operator string, ok bool) {
	if s.cfg.VerifyToken == nil {
		return "", true
	}
	return s.cfg.VerifyToken(r)
}

// loadTemplate 加载 HTML 模板（懒加载，线程安全）
func (s *Monitor) loadTemplate() (*template.Template, error) {
	s.parseOnce.Do(func() {
		s.template, s.parseErr = template.ParseFS(templateFS, "template.html")
	})
	return s.template, s.parseErr
}

// apiHandler 返回 JSON 格式的监控数据
func (s *Monitor) apiHandler(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"stages": s.stageData(),
		"custom": s.customData(),
	}
	if s.cfg.QueueStats != nil {
		resp["queues"] = s.cfg.QueueStats()
	}
	// 熔断状态跟着 /api/monitor 一起回：前端每轮刷新只发一个请求就能画出横幅，
	// 不必再单开一条轮询。单独的 GET /api/breaker 留给脚本/外部系统。
	if s.cfg.BreakerStatuses != nil {
		// 是**数组**不是单个对象：每个站点一条状态（暂停与否都在里面），后台要能逐站看、逐站放行。
		// 顺序固定（按站点名字典序），前端就不用再排。
		resp["breakers"] = sortedBreakerStatuses(s.cfg.BreakerStatuses())
	}
	if s.cfg.SiteStats != nil {
		resp["sites"] = s.cfg.SiteStats()
	}
	if s.cfg.SysInfo != nil {
		resp["system"] = s.cfg.SysInfo.Snapshot()
	}
	writeJSON(w, resp)
}

// stageData 组装各阶段统计（全局耗时统计 + worker 统计 + 队列计数）
//
// 键是**阶段键** `site:stage`（阶段名只在本站内唯一，两个站的 catalog 是两条），
// 条目里另给 `stage` / `site` 两个字段：前端按 `site` 分组、标题用 `stage`（短名字）。
func (s *Monitor) stageData() map[string]any {
	monitors := s.getter()
	out := make(map[string]any, len(monitors))
	for stage, ss := range monitors {
		out[stage] = map[string]any{
			// 裸阶段名（`GetStage()` 原值）与所属站点：站点的 Tab 靠 site 分组，
			// 标题显示 stage；键 `site:stage` 只用于唯一标识这一格。
			"stage":   ss.Stage,
			"site":    ss.Site,
			"global":  ss.Global,
			"workers": ss.Workers,
			"queue": map[string]any{
				"submitted":   ss.Queue.Submitted,
				"completed":   ss.Queue.Completed,
				"failed":      ss.Queue.Failed,
				"in_progress": ss.Queue.InProgress,
				"queue_len":   ss.Queue.QueueLen,
			},
		}
	}
	return out
}

// customData 返回业务自定义数据
func (s *Monitor) customData() map[string]any {
	if s.cfg.Metrics == nil {
		return map[string]any{}
	}
	return s.cfg.Metrics()
}

// settingsHandler 返回当前后台设置状态（不含密钥明文）
func (s *Monitor) settingsHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	// 用 append([]string{}, …) 而不是 append([]string(nil), …)：空名单要序列化成 []（前端按数组用），
	// nil 会变成 JSON null —— 老实现一直返回 []，别在这里退化成 null。
	wl := append([]string{}, s.rawWhitelist...)
	s.mu.RUnlock()

	// whitelist_source：这份白名单**持久化在哪**（空串 = 这个宿主没接配置存储，改动只在本次运行有效）。
	// 前端拿它拼那句提示 —— 原来这里报的是 whitelist_file / has_whitelist_file，白名单搬到库里之后
	// 那两个键没有意义了。
	source := ""
	if s.cfg.SaveWhitelist != nil {
		source = whitelistSource
	}
	writeJSON(w, map[string]any{
		"whitelist":        wl,
		"whitelist_source": source,
		"log_dir":          s.cfg.LogDir,
	})
}

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
func (s *Monitor) whitelistHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Whitelist []string `json:"whitelist"`
	}
	// 超限会以解码错误的形式浮上来（http.MaxBytesError），与"body 不是合法 JSON"合并成同一个 400
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	// 写时严格：先确认每一条都是 IP/CIDR，再去动存储。写坏一份白名单的代价是"限制静默放宽"，
	// 而那正是这条路唯一会造成的坏事 —— 当场拒收比事后从日志里翻出来强。
	if bad := firstInvalidWhitelistEntry(body.Whitelist); bad != "" {
		http.Error(w, "白名单里有不是 IP/CIDR 的条目："+bad, http.StatusBadRequest)
		return
	}
	if s.cfg.SaveWhitelist == nil {
		http.Error(w, "这个宿主没有接配置存储，白名单改不了（papa 由 crawler_config 表承载）", http.StatusNotFound)
		return
	}
	// 落库成功才动缓存：反过来的话，库写失败会留下一份"本进程放行、重启后不认"的白名单。
	if err := s.cfg.SaveWhitelist(body.Whitelist, auth.OperatorFrom(r.Context())); err != nil {
		s.logger.Errorf("save whitelist: %s", err.Error())
		http.Error(w, "保存白名单失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	s.SetWhitelist(body.Whitelist)
	writeJSON(w, map[string]any{"status": "ok", "whitelist": nonEmptyStrings(body.Whitelist)})
}

// shutdownHandler 优雅退出。
//
// 这是**高危操作**：要求调用方在请求体里把自己的访问令牌再输一遍（`{"token":"..."}`）。
// 中间件那道校验只证明"这个浏览器带着有效凭据"，这里要的是"人在场"——
// 误点、开着后台页面走开都不至于把服务停掉。（它不构成新的安全边界：令牌本来就是同一个，
// 作用是确认动作 + 记下操作人。）
func (s *Monitor) shutdownHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.OnShutdown == nil || s.cfg.VerifyTokenValue == nil {
		http.Error(w, "shutdown not configured", http.StatusNotFound)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	operator, ok := s.cfg.VerifyTokenValue(body.Token)
	if !ok {
		s.logger.Errorf("拒绝关停：令牌校验不通过（来源 %s）", clientIP(r))
		http.Error(w, "访问令牌不正确", http.StatusForbidden)
		return
	}
	if operator != "" {
		s.logger.Errorf("收到优雅退出请求，操作人 %s（来源 %s）", operator, clientIP(r))
	} else {
		s.logger.Errorf("收到优雅退出请求（未配令牌，来源 %s）", clientIP(r))
	}
	writeJSON(w, map[string]any{"status": "shutting down"})
	time.AfterFunc(500*time.Millisecond, s.cfg.OnShutdown)
}

// auditQueueProcess 把一次「立即执行」的结局交给宿主记审计。
//
// 只记**真的跑过**的那种：接口没配置（404）属于"压根没这条功能"，不是一次触发，不记。
// 操作人与 IP 在这里取（与白名单同一套取法），宿主只负责落到 oplog。
func (s *Monitor) auditQueueProcess(r *http.Request, queue string, processed int, err error) {
	if s.cfg.OnQueueProcess == nil {
		return
	}
	s.cfg.OnQueueProcess(QueueProcessEvent{
		Queue:     queue,
		Operator:  auth.OperatorFrom(r.Context()),
		IP:        clientIP(r).String(),
		Processed: processed,
		Err:       err,
		At:        time.Now(),
	})
}

// errorQueueProcessHandler 手动触发失败任务错误队列处理
func (s *Monitor) errorQueueProcessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ProcessErrorQueue == nil {
		http.Error(w, "error queue not configured", http.StatusNotFound)
		return
	}
	count, err := s.cfg.ProcessErrorQueue()
	s.auditQueueProcess(r, engine.QueueError, count, err)
	if err != nil {
		s.logger.Errorf("process error queue: %s", err.Error())
		http.Error(w, "process error queue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "processed": count})
}

// repeatQueueProcessHandler 手动触发周期轮询队列处理
func (s *Monitor) repeatQueueProcessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ProcessRepeatQueue == nil {
		http.Error(w, "repeat queue not configured", http.StatusNotFound)
		return
	}
	count, err := s.cfg.ProcessRepeatQueue()
	s.auditQueueProcess(r, engine.QueueRepeat, count, err)
	if err != nil {
		s.logger.Errorf("process repeat queue: %s", err.Error())
		http.Error(w, "process repeat queue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "repolled": count})
}

// sortedBreakerStatuses 把各站点的状态排成稳定顺序（按站点名字典序）。
func sortedBreakerStatuses(all map[string]core.BreakerStatus) []core.BreakerStatus {
	sites := make([]string, 0, len(all))
	for site := range all {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	out := make([]core.BreakerStatus, 0, len(sites))
	for _, site := range sites {
		st := all[site]
		st.Site = site // 键就是站点，别让调用方再从 map 键去猜
		out = append(out, st)
	}
	return out
}

// breakerHandler 读各 scope 的熔断状态。写操作见 breakerResumeHandler。
func (s *Monitor) breakerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"breakers": sortedBreakerStatuses(s.cfg.BreakerStatuses())})
}

// breakerResumeHandler 手动放行暂停中的抓取（手动暂停的、熔断触发的，同一个按钮）。
//
// 这是**人在场的干预**，所以成功了要记进操作日志（OnBreakerResume 回调，宿主接到 oplog）。
// 已经是运行态时返回 resumed=false 而不是报错 —— 「重复点击恢复」不该看起来像失败，
// 但也不能回一个含糊的 ok 让调用方以为是自己放行的。
func (s *Monitor) breakerResumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ResumeBreaker == nil || s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}

	// 要放行谁：`?scope=xxx&stage=yyy` 或请求体同名字段；都不给 = **全部站点**（整站那一层）。
	// 多站时"全部"是横幅上那个「恢复抓取」，单站时与以前完全一样。
	site, stage, ok := breakerTargetParam(r)
	if !ok {
		http.Error(w, "bad request: scope/stage must be strings (stage 需要 scope)", http.StatusBadRequest)
		return
	}
	known := s.cfg.BreakerStatuses()
	if site != "" {
		if _, exists := known[site]; !exists {
			http.Error(w, "unknown scope: "+site, http.StatusBadRequest)
			return
		}
	}

	targets := []string{site}
	if site == "" {
		targets = targets[:0]
		for key := range known {
			targets = append(targets, key)
		}
		sort.Strings(targets)
	}
	resumed := make([]string, 0, len(targets))
	for _, key := range targets {
		if s.cfg.ResumeBreaker(key, stage) {
			resumed = append(resumed, key)
		}
	}
	if len(resumed) == 0 {
		writeJSON(w, map[string]any{"status": "ok", "resumed": false, "note": "当前不在暂停态"})
		return
	}
	operator := auth.OperatorFrom(r.Context())
	for _, key := range resumed {
		if s.cfg.OnBreakerResume != nil {
			s.cfg.OnBreakerResume(operator, key, stage)
		}
	}
	s.logger.Infof("已手动放行（操作人 %q，scope=%q stage=%q），恢复取任务", operator, strings.Join(resumed, ","), stage)
	// resumed 仍是**布尔**（前端/脚本靠它区分"本来就没暂停"）；放行了哪些 scope 在 resumed_scopes 里。
	out := map[string]any{"status": "ok", "resumed": true, "resumed_scopes": resumed}
	if stage != "" {
		out["stage"] = stage
	}
	writeJSON(w, out)
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
func (s *Monitor) breakerPauseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.PauseBreaker == nil || s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	site, stage, ok := breakerTargetParam(r)
	if !ok {
		http.Error(w, "bad request: scope/stage must be strings (stage 需要 scope)", http.StatusBadRequest)
		return
	}
	if site == "" {
		// 与 resume 的默认相反：这里不点名就是错的（见函数头）。文案要说清"不是静默停全部"。
		http.Error(w, "bad request: scope is required（暂停必须点名站点；不点名不会停掉全部）", http.StatusBadRequest)
		return
	}
	if _, exists := s.cfg.BreakerStatuses()[site]; !exists {
		http.Error(w, "unknown scope: "+site, http.StatusBadRequest)
		return
	}
	if !s.cfg.PauseBreaker(site, stage) {
		// 本来就在暂停态：不报错（重复点击不该看起来像失败），也不记审计（没有状态变更）——
		// 与 resume 的 resumed=false 那条同一取向。
		note := "该站点本来就在暂停态"
		if stage != "" {
			note = "该站点或这个阶段本来就在暂停态"
		}
		writeJSON(w, map[string]any{"status": "ok", "paused": false, "note": note})
		return
	}
	operator := auth.OperatorFrom(r.Context())
	if s.cfg.OnBreakerPause != nil {
		s.cfg.OnBreakerPause(operator, site, stage)
	}
	s.logger.Infof("已手动暂停（操作人 %q，scope=%q stage=%q），worker 不再取任务", operator, site, stage)
	out := map[string]any{"status": "ok", "paused": true, "paused_scopes": []string{site}}
	if stage != "" {
		out["stage"] = stage
	}
	writeJSON(w, out)
}

// breakerTargetParam 取暂停/放行的对象：先看 `?scope=&stage=`，再看请求体 `{"scope":"…","stage":"…"}`。
//
// `scope` 是**站点键**（沿用这个端点的老叫法）；空串在 resume 里表示"全部站点"、在 pause 里是错误。
// `stage` 为空 = 整站那一层。**给了 stage 却没给 scope 一律不合法**（阶段属于某个站）——
// 调用方回 400，别静默当成"整站"或"全部"。
func breakerTargetParam(r *http.Request) (site, stage string, ok bool) {
	q := r.URL.Query()
	site, stage = q.Get("scope"), q.Get("stage")
	if r.Body != nil {
		var body struct {
			Scope *string `json:"scope"`
			Stage *string `json:"stage"`
		}
		// 空请求体是合法的（"全部放行"）：先看一眼有没有内容，避免把 io.EOF 当解析失败。
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10))
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return "", "", false
		}
		if body.Scope != nil {
			site = *body.Scope
		}
		if body.Stage != nil {
			stage = *body.Stage
		}
	}
	if stage != "" && site == "" {
		return "", "", false
	}
	return site, stage, true
}

// taskTraceHandler 返回一条任务的步骤追踪时间线（按尝试、步骤排序）。
func (s *Monitor) taskTraceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.TaskTrace == nil {
		http.Error(w, "task trace not configured", http.StatusNotFound)
		return
	}
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil || id <= 0 {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	steps, err := s.cfg.TaskTrace(id)
	if err != nil {
		// 业务原因（如追踪开关没开）原样回给前端，抽屉里直接显示这句话
		s.logger.Errorf("load task trace %d: %s", id, err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"task_id": id, "steps": steps})
}

// logFile 日志文件条目
type logFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// taskPageHandler 下载一个归档页面（`GET /api/task/page?file=<相对路径>`）。
//
// 给的是**文件本身**（`Content-Disposition: attachment`）而不是内联渲染 —— 拿下去本地分析
// （对着真实页面写选择器）比在浏览器里看更实用，也省掉"注入 base 标签才能长回原样"那套。
func (s *Monitor) taskPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ArchiveDir == "" {
		http.Error(w, "页面归档未开启（crawler.archive.enabled）", http.StatusNotFound)
		return
	}
	rel := r.URL.Query().Get("file")
	abs, err := resolveArchivedFile(s.cfg.ArchiveDir, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, "归档文件读不到（可能已过保留期）", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(abs)+"\"")
	_, _ = w.Write(b)
}

// resolveArchivedFile 把一个**相对路径**解析到归档根目录下的绝对路径；越界一律拒绝。
//
// 两道判据：先按路径语义清洗（拒绝绝对路径与 `..`），再确认解析结果确实落在根之下
// （防大小写/分隔符之类的意外）。归档目录里的文件名由框架生成，但路径是**从请求里来的** ——
// 那是不可信输入，这里不靠"文件名不含 .."这种假设。
func resolveArchivedFile(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("缺少 file 参数")
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("file 参数非法")
	}
	// 明确拒掉"看起来就是绝对路径"的输入：Windows 上 `filepath.IsAbs("\etc\passwd")` 是 **false**
	//（没有盘符不算绝对路径），光靠 IsAbs 会把它当相对路径拼进根目录 —— 拼进去虽然仍在根下，
	// 但那种输入本来就不该被接受。带盘符的（`C:…`）同理。
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") ||
		(len(rel) >= 2 && rel[1] == ':') {
		return "", fmt.Errorf("file 参数越界")
	}
	cleaned := filepath.Clean(filepath.FromSlash(rel))
	// `example/..` 清洗完是 `.`（指到根目录本身）—— 那不是文件，也别让它再往下走。
	if cleaned == "." || filepath.IsAbs(cleaned) || cleaned == ".." ||
		strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file 参数越界")
	}
	abs := filepath.Join(root, cleaned)
	inside, err := filepath.Rel(root, abs)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file 参数越界")
	}
	return abs, nil
}

// logsListHandler 列出日志目录下的文件
func (s *Monitor) logsListHandler(w http.ResponseWriter, r *http.Request) {
	files := []logFile{}
	if s.cfg.LogDir != "" {
		if entries, err := os.ReadDir(s.cfg.LogDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if info, err := e.Info(); err == nil {
					files = append(files, logFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
				}
			}
		}
	}
	writeJSON(w, map[string]any{"log_dir": s.cfg.LogDir, "files": files})
}

// logsDownloadHandler 下载单个日志文件（?file=name），缺省打包全部为 zip
func (s *Monitor) logsDownloadHandler(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.LogDir
	if dir == "" {
		http.Error(w, "log dir not configured", http.StatusNotFound)
		return
	}
	name := filepath.Base(r.URL.Query().Get("file"))
	if name != "" && name != "." && name != ".." && name != string(filepath.Separator) {
		http.ServeFile(w, r, filepath.Join(dir, name))
		return
	}
	zipLogs(w, dir)
}

// zipLogs 将日志目录下所有文件打包为 zip 响应
func zipLogs(w http.ResponseWriter, dir string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="logs.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		fw, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return nil
		}
		_, _ = io.Copy(fw, src)
		_ = src.Close()
		return nil
	})
}

// noCache 禁止静态资源被浏览器缓存，改样式后无需手动清缓存即可看到。
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// NoCache 导出 noCache，供宿主挂载其它静态资源（如 oao 组件）时复用。
func NoCache(next http.Handler) http.Handler { return noCache(next) }

// writeJSON 统一写 JSON 响应
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fileExists 判断路径是否存在
// htmlHandler 返回 HTML 监控页面
func (s *Monitor) htmlHandler(w http.ResponseWriter, r *http.Request) {
	tmpl, err := s.loadTemplate()
	if err != nil {
		s.logger.Errorf("load monitor template: %s", err.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, nil); err != nil {
		s.logger.Errorf("execute monitor template: %s", err.Error())
	}
}
