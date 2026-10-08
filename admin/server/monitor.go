package server

import (
	"archive/zip"
	"embed"
	"encoding/json"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v2/admin/auth"
	"github.com/ydtg1993/papa/v2/admin/sysinfo"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/core"
)

//go:embed template.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// MonitorGetter 定义获取所有阶段统计快照的函数类型
type MonitorGetter func() map[string]core.StageStats

// MonitorConfig 监控服务配置
type MonitorConfig struct {
	// VerifyToken 校验访问令牌，返回（操作人, 是否通过）；为 nil 表示不校验（未配置凭据）。
	// 由宿主注入（papa 用 admin/auth 的库表实现），本包不认识令牌怎么存 ——
	// 这样这段安全关键逻辑可以用桩离线测，包也不必依赖 gorm。
	VerifyToken        func(r *http.Request) (operator string, ok bool)
	Whitelist          []string                               // 初始 IP/CIDR 白名单，空=不限制
	WhitelistFile      string                                 // 白名单持久化文件路径（动态更新时写回）
	Metrics            func() map[string]any                  // 业务自定义数据快照（可空）
	QueueStats         func() map[string]core.QueueStat       // 治理队列运行快照（可空）
	SysInfo            *sysinfo.Collector                     // 系统指标采集器（可空）
	LogDir             string                                 // 日志目录（导出用）
	OnShutdown         func()                                 // 优雅退出回调
	ProcessErrorQueue  func() (int, error)                    // 错误队列手动触发回调（可空）
	ProcessRepeatQueue func() (int, error)                    // 周期轮询队列手动触发回调（可空）
	ConfigGet          func() *config.RuntimeConfig           // 返回当前运行期覆盖层（可空）
	ConfigSet          func(*config.RuntimeConfig) error      // 应用运行期覆盖层（可空）
	TaskTrace          func(id int) ([]core.TraceStep, error) // 单任务步骤追踪（可空）
	// BreakerStatus 熔断闸门的状态快照（可空；也随 /api/monitor 一起返回）。
	BreakerStatus func() core.BreakerStatus
	// ResumeBreaker 手动放行被闸住的抓取；返回是否真的从暂停态切了回来（本来在跑就是 false）。
	ResumeBreaker func() bool
	// OnBreakerResume 放行成功后的回调，宿主拿它记操作日志（人在场的干预必须留痕）；可空。
	OnBreakerResume func(operator string)
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

	mu        sync.RWMutex
	whitelist []*net.IPNet
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
		getter:    getter,
		logger:    logger,
		cfg:       cfg,
		whitelist: parseWhitelist(cfg.Whitelist, logger),
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
	return out
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
	mux.HandleFunc("/api/config", s.wrap(s.configHandler))
	mux.HandleFunc("/api/task/trace", s.wrap(s.taskTraceHandler))
	mux.HandleFunc("/api/logs", s.wrap(s.logsListHandler))
	mux.HandleFunc("/api/logs/download", s.wrap(s.logsDownloadHandler))
}

// wrap 包装处理器：先 IP 白名单，再令牌校验（仅 /api/ 数据接口）。
// 校验通过时把「操作人」放进请求上下文，操作日志据此记人。
func (s *Monitor) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ipAllowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// 带凭据的数据响应不许任何缓存留存：令牌被停用/删除后，代理（或浏览器）里那份
			// 旧 200 还能被原样重放出来。浏览器自己不会缓存带 Authorization 的响应，
			// 但中间代理会 —— 这里把它明确掉，不靠实现细节。
			w.Header().Set("Cache-Control", "no-store")
			operator, ok := s.authOK(r)
			if !ok {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(auth.WithOperator(r.Context(), operator))
		}
		next(w, r)
	}
}

// Auth 把同一套「白名单 + 令牌」校验暴露成 http.Handler 中间件，
// 供挂在同一 mux 上的外部组件（如 oao 表格）复用，避免业务侧接口绕过鉴权。
func (s *Monitor) Auth(next http.Handler) http.Handler {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
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
	if s.cfg.BreakerStatus != nil {
		resp["breaker"] = s.cfg.BreakerStatus()
	}
	if s.cfg.SysInfo != nil {
		resp["system"] = s.cfg.SysInfo.Snapshot()
	}
	writeJSON(w, resp)
}

// stageData 组装各阶段统计（全局耗时统计 + worker 统计 + 队列计数）
func (s *Monitor) stageData() map[string]any {
	monitors := s.getter()
	out := make(map[string]any, len(monitors))
	for stage, ss := range monitors {
		out[stage] = map[string]any{
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
	wl := make([]string, len(s.whitelist))
	for i, n := range s.whitelist {
		wl[i] = n.String()
	}
	s.mu.RUnlock()

	writeJSON(w, map[string]any{
		"whitelist":          wl,
		"whitelist_file":     s.cfg.WhitelistFile,
		"has_whitelist_file": s.cfg.WhitelistFile != "" && fileExists(s.cfg.WhitelistFile),
		"log_dir":            s.cfg.LogDir,
	})
}

// maxAdminBody 后台几个收 JSON 的接口的请求体上限。
//
// 它们的 body 都只是几个短字段（白名单数组 / 运行期覆盖层），但**必须有个头**：
// `json.Decoder` 会把一个没写完的 JSON（比如一个永远不闭合的数组）一路读下去，
// 不设限就是任人喂内存 —— 这条路径还在鉴权后面，但那是"要令牌"，不是"不用管"。
// 64KB 约合 3000 条 IP/CIDR，给足余量。
const maxAdminBody = 64 << 10

// whitelistHandler 动态更新 IP/CIDR 白名单并持久化到 whitelist_file
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
	if s.cfg.WhitelistFile != "" {
		data := strings.Join(body.Whitelist, "\n")
		if err := os.WriteFile(s.cfg.WhitelistFile, []byte(data+"\n"), 0o600); err != nil {
			s.logger.Errorf("write whitelist file %s: %s", s.cfg.WhitelistFile, err.Error())
			http.Error(w, "persist whitelist failed", http.StatusInternalServerError)
			return
		}
	}
	parsed := parseWhitelist(body.Whitelist, s.logger)
	s.mu.Lock()
	s.whitelist = parsed
	s.mu.Unlock()
	writeJSON(w, map[string]any{"status": "ok", "whitelist": body.Whitelist})
}

// shutdownHandler 触发优雅退出（稍延迟以便响应刷出）
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
	if err != nil {
		s.logger.Errorf("process repeat queue: %s", err.Error())
		http.Error(w, "process repeat queue failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "repolled": count})
}

// breakerHandler 读熔断闸门的状态。写操作见 breakerResumeHandler。
func (s *Monitor) breakerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.BreakerStatus == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, s.cfg.BreakerStatus())
}

// breakerResumeHandler 手动放行被熔断闸住的抓取。
//
// 这是**人在场的干预**，所以成功了要记进操作日志（OnBreakerResume 回调，宿主接到 oplog）。
// 已经是运行态时返回 resumed=false 而不是报错 —— 「重复点击恢复」不该看起来像失败，
// 但也不能回一个含糊的 ok 让调用方以为是自己放行的。
func (s *Monitor) breakerResumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ResumeBreaker == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	if !s.cfg.ResumeBreaker() {
		writeJSON(w, map[string]any{"status": "ok", "resumed": false, "note": "当前不在暂停态"})
		return
	}
	operator := auth.OperatorFrom(r.Context())
	if s.cfg.OnBreakerResume != nil {
		s.cfg.OnBreakerResume(operator)
	}
	s.logger.Infof("熔断已手动放行（操作人 %q），爬虫恢复取任务", operator)
	writeJSON(w, map[string]any{"status": "ok", "resumed": true})
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

// hotReloadableFields 可热更字段（供后台 UI 标注）。
var hotReloadableFields = []string{
	"browser.max_idle_time", "browser.headers",
	"html.timeout", "html.max_body_size", "html.headers",
	"error_queue.enabled", "error_queue.interval", "error_queue.worker_count", "error_queue.max_retry", "error_queue.batch_size",
	"recover_queue.enabled", "recover_queue.worker_count", "recover_queue.batch_size",
	"repeat_queue.enabled", "repeat_queue.interval", "repeat_queue.worker_count", "repeat_queue.batch_size",
}

// restartOnlyFields 需重启才生效的字段（PUT 时会被拒绝）。
var restartOnlyFields = []string{
	"browser.enable", "browser.headless", "browser.no_sandbox", "browser.leakless", "browser.browser_path",
	"proxy.api_url", "proxy.refresh_interval",
	"core.stages", "core.dedup_cache_size",
}

// configHandler 查询/更新运行期动态配置。
func (s *Monitor) configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.configGet(w)
	case http.MethodPut:
		s.configPut(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Monitor) configGet(w http.ResponseWriter) {
	if s.cfg.ConfigGet == nil {
		http.Error(w, "config not available", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"overrides":           s.cfg.ConfigGet(),
		"hot_fields":          hotReloadableFields,
		"restart_only_fields": restartOnlyFields,
	})
}

func (s *Monitor) configPut(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ConfigSet == nil {
		http.Error(w, "config not available", http.StatusNotFound)
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	dec.DisallowUnknownFields()
	var rt config.RuntimeConfig
	if err := dec.Decode(&rt); err != nil {
		http.Error(w, "invalid config: "+err.Error()+" (仅支持热更字段)", http.StatusBadRequest)
		return
	}
	for name, v := range map[string]*int{
		"error_queue.worker_count":   rt.ErrorQueue.WorkerCount,
		"error_queue.max_retry":      rt.ErrorQueue.MaxRetry,
		"error_queue.batch_size":     rt.ErrorQueue.BatchSize,
		"recover_queue.worker_count": rt.RecoverQueue.WorkerCount,
		"recover_queue.batch_size":   rt.RecoverQueue.BatchSize,
		"repeat_queue.worker_count":  rt.RepeatQueue.WorkerCount,
		"repeat_queue.batch_size":    rt.RepeatQueue.BatchSize,
	} {
		if v != nil && *v < 0 {
			http.Error(w, name+" 必须 >= 0", http.StatusBadRequest)
			return
		}
	}
	if err := s.cfg.ConfigSet(&rt); err != nil {
		s.logger.Errorf("apply config: %s", err.Error())
		http.Error(w, "apply config failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok"})
}

// logFile 日志文件条目
type logFile struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
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
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

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
