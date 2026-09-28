package server

import (
	"archive/zip"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v2/crawler"
	"github.com/ydtg1993/papa/v2/pkg/metrics"
	"github.com/ydtg1993/papa/v2/pkg/sysinfo"
	"github.com/ydtg1993/papa/v2/pkg/track"
)

//go:embed template.html
var templateFS embed.FS

// MonitorGetter 定义获取所有阶段监控器的函数类型
type MonitorGetter func() map[string]*track.StatsQueue[*crawler.Task]

// MonitorConfig 监控服务配置
type MonitorConfig struct {
	AuthKey       string             // 初始访问密钥，空=不校验
	AuthKeyFile   string             // 密钥文件路径（重新生成时持久化到此文件）
	Whitelist     []string           // 初始 IP/CIDR 白名单，空=不限制
	WhitelistFile string             // 白名单持久化文件路径（动态更新时写回）
	Metrics       *metrics.Registry  // 业务自定义数据（可空）
	SysInfo       *sysinfo.Collector // 系统指标采集器（可空）
	LogDir        string             // 日志目录（导出用）
	OnShutdown    func()             // 优雅退出回调
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
	authKey   string
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
		authKey:   cfg.AuthKey,
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
	mux.HandleFunc("/monitor", s.wrap(s.htmlHandler))
	mux.HandleFunc("/api/monitor", s.wrap(s.apiHandler))
	mux.HandleFunc("/api/settings", s.wrap(s.settingsHandler))
	mux.HandleFunc("/api/settings/whitelist", s.wrap(s.whitelistHandler))
	mux.HandleFunc("/api/settings/secret", s.wrap(s.secretHandler))
	mux.HandleFunc("/api/settings/shutdown", s.wrap(s.shutdownHandler))
	mux.HandleFunc("/api/logs", s.wrap(s.logsListHandler))
	mux.HandleFunc("/api/logs/download", s.wrap(s.logsDownloadHandler))
}

// wrap 包装处理器：先 IP 白名单，再密钥校验（仅 /api/ 数据接口）
func (s *Monitor) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.ipAllowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.authOK(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
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

// authOK 校验密钥；密钥为空则放行
func (s *Monitor) authOK(r *http.Request) bool {
	s.mu.RLock()
	key := s.authKey
	s.mu.RUnlock()
	if key == "" {
		return true
	}
	k := extractKey(r)
	return k != "" && k == key
}

// extractKey 从 Authorization: Bearer / X-Auth-Key / ?key= 提取密钥
func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if h := r.Header.Get("X-Auth-Key"); h != "" {
		return h
	}
	return r.URL.Query().Get("key")
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
	if s.cfg.SysInfo != nil {
		resp["system"] = s.cfg.SysInfo.Snapshot()
	}
	writeJSON(w, resp)
}

// stageData 组装各阶段统计（全局耗时统计 + worker 统计 + 队列计数）
func (s *Monitor) stageData() map[string]any {
	monitors := s.getter()
	out := make(map[string]any, len(monitors))
	for stage, mon := range monitors {
		submitted, completed, failed, inProgress, queueLen := mon.WorkPool.Stats()
		out[stage] = map[string]any{
			"global":  mon.GetGlobalStats(),
			"workers": mon.GetAllWorkerStats(),
			"queue": map[string]any{
				"submitted":   submitted,
				"completed":   completed,
				"failed":      failed,
				"in_progress": inProgress,
				"queue_len":   queueLen,
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
	return s.cfg.Metrics.GetAll()
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
		"auth_key_file":      s.cfg.AuthKeyFile,
		"has_secret_file":    s.cfg.AuthKeyFile != "" && fileExists(s.cfg.AuthKeyFile),
		"log_dir":            s.cfg.LogDir,
	})
}

// whitelistHandler 动态更新 IP/CIDR 白名单并持久化到 whitelist_file
func (s *Monitor) whitelistHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Whitelist []string `json:"whitelist"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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

// secretHandler 重新生成密钥并持久化到密钥文件
func (s *Monitor) secretHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key, err := generateSecret()
	if err != nil {
		http.Error(w, "generate secret failed", http.StatusInternalServerError)
		return
	}
	if s.cfg.AuthKeyFile != "" {
		if err := os.WriteFile(s.cfg.AuthKeyFile, []byte(key+"\n"), 0o600); err != nil {
			s.logger.Errorf("write secret file %s: %s", s.cfg.AuthKeyFile, err.Error())
			http.Error(w, "write secret file failed", http.StatusInternalServerError)
			return
		}
	}
	s.mu.Lock()
	s.authKey = key
	s.mu.Unlock()
	writeJSON(w, map[string]any{"key": key})
}

// shutdownHandler 触发优雅退出（稍延迟以便响应刷出）
func (s *Monitor) shutdownHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{"status": "shutting down"})
	if s.cfg.OnShutdown != nil {
		time.AfterFunc(500*time.Millisecond, s.cfg.OnShutdown)
	}
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
		defer src.Close()
		_, _ = io.Copy(fw, src)
		return nil
	})
}

// generateSecret 生成 32 字节随机密钥的十六进制串
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

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
