package access

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"net"
	"net/http"
	"strings"
	"sync"
)

type Logger interface{ Errorf(string, ...any) }
type Options struct {
	VerifyToken   func(*http.Request) (string, bool)
	AuthRequired  func() bool
	SaveWhitelist func([]string, string) error
	Whitelist     []string
}
type Handler struct {
	cfg          Options
	logger       Logger
	mu           sync.RWMutex
	whitelist    []*net.IPNet
	rawWhitelist []string
}

func New(cfg Options, logger Logger) *Handler {
	return &Handler{cfg: cfg, logger: logger, whitelist: ParseWhitelist(cfg.Whitelist, logger), rawWhitelist: NonEmptyStrings(cfg.Whitelist)}
}

const maxAdminBody = 64 << 10
const whitelistSource = "crawler_config.ip_whitelist"

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func ParseWhitelist(items []string, logger Logger) []*net.IPNet {
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

func NonEmptyStrings(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Handler) SetWhitelist(entries []string) {
	parsed := ParseWhitelist(entries, s.logger)
	raw := NonEmptyStrings(entries)
	s.mu.Lock()
	s.whitelist = parsed
	s.rawWhitelist = raw
	s.mu.Unlock()
}

func FirstInvalidWhitelistEntry(items []string) string {
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

func (s *Handler) Wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.IPAllowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			r2, ok := s.RequireToken(w, r)
			if !ok {
				return
			}
			r = r2
		}
		next(w, r)
	}
}

func (s *Handler) RequireToken(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	w.Header().Set("Cache-Control", "no-store")
	operator, ok := s.AuthOK(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return r, false
	}
	return r.WithContext(auth.WithOperator(r.Context(), operator)), true
}

func (s *Handler) Auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.IPAllowed(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		r2, ok := s.RequireToken(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r2)
	})
}

func (s *Handler) IPAllowed(r *http.Request) bool {
	s.mu.RLock()
	wl := s.whitelist
	s.mu.RUnlock()
	if len(wl) == 0 {
		return true
	}
	ip := ClientIP(r)
	for _, n := range wl {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func ClientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func (s *Handler) AuthOK(r *http.Request) (operator string, ok bool) {
	if s.cfg.VerifyToken == nil {
		return "", false
	}
	return s.cfg.VerifyToken(r)
}

func (s *Handler) AuthRequired() bool {
	if s.cfg.AuthRequired != nil {
		return s.cfg.AuthRequired()
	}
	return true
}

func (s *Handler) Whitelist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Whitelist *[]string `json:"whitelist"`
	}
	// 超限会以解码错误的形式浮上来（http.MaxBytesError），与"body 不是合法 JSON"合并成同一个 400
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	// 字段缺失或显式 null 都**不是**"清空"：空列表在 IPAllowed 里等于"不限制来源"，是放宽而不是
	// 收紧 —— 一次字段名写错或手抖的请求不该悄悄放开限制。想清空必须显式传 []。
	if body.Whitelist == nil {
		http.Error(w, "whitelist 必须是数组（想清空请显式传 []）", http.StatusBadRequest)
		return
	}
	whitelist := *body.Whitelist
	// 写时严格：先确认每一条都是 IP/CIDR，再去动存储。写坏一份白名单的代价是"限制静默放宽"，
	// 而那正是这条路唯一会造成的坏事 —— 当场拒收比事后从日志里翻出来强。
	if bad := FirstInvalidWhitelistEntry(whitelist); bad != "" {
		http.Error(w, "白名单里有不是 IP/CIDR 的条目："+bad, http.StatusBadRequest)
		return
	}
	if s.cfg.SaveWhitelist == nil {
		http.Error(w, "这个宿主没有接配置存储，白名单改不了（papa 由 crawler_config 表承载）", http.StatusNotFound)
		return
	}
	// 落库成功才动缓存：反过来的话，库写失败会留下一份"本进程放行、重启后不认"的白名单。
	if err := s.cfg.SaveWhitelist(whitelist, auth.OperatorFrom(r.Context())); err != nil {
		s.logger.Errorf("save whitelist: %s", err.Error())
		http.Error(w, "保存白名单失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	s.SetWhitelist(whitelist)
	writeJSON(w, map[string]any{"status": "ok", "whitelist": NonEmptyStrings(whitelist)})
}

// WhitelistSnapshot returns a detached copy; access alone owns the effective cache.
func (s *Handler) WhitelistSnapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string{}, s.rawWhitelist...)
}
