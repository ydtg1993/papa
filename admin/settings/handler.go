package settings

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/admin/access"
	"net/http"
	"time"
)

type Logger interface{ Errorf(string, ...any) }
type Options struct {
	Whitelist           func() []string
	WhitelistPersistent bool
	LogDir              string
	OnShutdown          func()
	VerifyTokenValue    func(string) (string, bool)
}
type Handler struct {
	cfg    Options
	logger Logger
}

func New(cfg Options, logger Logger) *Handler { return &Handler{cfg: cfg, logger: logger} }
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Handler) Settings(w http.ResponseWriter, r *http.Request) {
	wl := s.cfg.Whitelist()

	// whitelist_source：这份白名单**持久化在哪**（空串 = 这个宿主没接配置存储，改动只在本次运行有效）。
	// 前端拿它拼那句提示 —— 原来这里报的是 whitelist_file / has_whitelist_file，白名单搬到库里之后
	// 那两个键没有意义了。
	source := ""
	if s.cfg.WhitelistPersistent {
		source = "crawler_config.ip_whitelist"
	}
	writeJSON(w, map[string]any{
		"whitelist":        wl,
		"whitelist_source": source,
		"log_dir":          s.cfg.LogDir,
	})
}
func (s *Handler) Shutdown(w http.ResponseWriter, r *http.Request) {
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
		s.logger.Errorf("拒绝关停：令牌校验不通过（来源 %s）", access.ClientIP(r))
		http.Error(w, "访问令牌不正确", http.StatusForbidden)
		return
	}
	if operator != "" {
		s.logger.Errorf("收到优雅退出请求，操作人 %s（来源 %s）", operator, access.ClientIP(r))
	} else {
		s.logger.Errorf("收到优雅退出请求（未配令牌，来源 %s）", access.ClientIP(r))
	}
	writeJSON(w, map[string]any{"status": "shutting down"})
	time.AfterFunc(500*time.Millisecond, s.cfg.OnShutdown)
}
