package tokenadmin

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

// maxBody 请求体上限：这几条接口的 body 只有几个短字段，别让人用一个畸形请求吃光内存。
const maxBody = 4 << 10

// Event 一次令牌写操作的结局，供宿主记审计（成功与失败都会回调）。
type Event struct {
	Table  string         // 固定 TableKey
	Action string         // create / enable / disable / remove
	ID     uint           // 目标令牌 ID（create 是新建出来的那条）
	Values map[string]any // 提交值 —— **不放明文令牌**
	Err    error          // nil 表示成功
	IP     string
	At     time.Time
	Req    *http.Request
}

// Hook 宿主对写操作的接线（papa 用它写操作日志）。可为 nil。
type Hook func(Event)

// Logger 只要一个 Errorf —— 避免为一个日志接口把宿主拉进来。
type Logger interface {
	Errorf(format string, args ...any)
}

// API 「访问令牌」页的三条接口（GET/POST 同路径）。
type API struct {
	store Store
	hook  Hook
	log   Logger
}

// NewAPI 创建接口层；hook 与 log 都可以为 nil。
func NewAPI(store Store, hook Hook, log Logger) *API {
	return &API{store: store, hook: hook, log: log}
}

// Register 把路由挂到 mux 上。wrap 传监控后台的中间件（mon.Auth）——
// 令牌管理接口本身也是 /api/*，同样要过白名单与令牌校验。
func (a *API) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("/api/tokens", wrap(http.HandlerFunc(a.handleTokens)))
	mux.Handle("/api/tokens/enabled", wrap(http.HandlerFunc(a.handleEnabled)))
	mux.Handle("/api/tokens/remove", wrap(http.HandlerFunc(a.handleRemove)))
}

// handleTokens：GET 列表，POST 新增。
// 新增成功时**把明文令牌回给前端一次**（库里只有哈希，过后无从显示）。
func (a *API) handleTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.store.List()
		if err != nil {
			a.failBody(w, r, err, "list")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": rows})

	case http.MethodPost:
		var body struct {
			Operator string `json:"operator"`
			Note     string `json:"note"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		token, id, err := a.store.Create(body.Operator, body.Note)
		// 审计里只留操作人与备注，**不记明文令牌**
		a.changed("create", id, map[string]any{"operator": body.Operator, "note": body.Note}, err, r)
		if err != nil {
			a.failBody(w, r, err, "create")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "token": token})

	default:
		methodNotAllowed(w)
	}
}

// handleEnabled 停用 / 启用：{id, enabled}。
func (a *API) handleEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var body struct {
		ID      uint `json:"id"`
		Enabled bool `json:"enabled"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.ID == 0 {
		writeError(w, http.StatusBadRequest, "缺少令牌 ID")
		return
	}
	action := "disable"
	if body.Enabled {
		action = "enable"
	}
	err := a.store.SetEnabled(body.ID, body.Enabled)
	a.changed(action, body.ID, map[string]any{"enabled": body.Enabled}, err, r)
	if err != nil {
		a.failBody(w, r, err, action)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleRemove 删除：{id}。
func (a *API) handleRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var body struct {
		ID uint `json:"id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.ID == 0 {
		writeError(w, http.StatusBadRequest, "缺少令牌 ID")
		return
	}
	err := a.store.Delete(body.ID)
	a.changed("remove", body.ID, nil, err, r)
	if err != nil {
		a.failBody(w, r, err, "remove")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// changed 回调宿主（记审计）。
func (a *API) changed(action string, id uint, values map[string]any, err error, r *http.Request) {
	if a.hook == nil {
		return
	}
	a.hook(Event{
		Table: TableKey, Action: action, ID: id, Values: values,
		Err: err, IP: clientIP(r), At: time.Now(), Req: r,
	})
}

// failBody 把存储层的哨兵错误映射成状态码；其它错误按 500 记日志。
func (a *API) failBody(w http.ResponseWriter, r *http.Request, err error, action string) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrChanged):
		status = http.StatusConflict
	case errors.Is(err, ErrOperatorRequired):
		status = http.StatusBadRequest
	}
	if status == http.StatusInternalServerError && a.log != nil {
		a.log.Errorf("tokenadmin: %s %s (%s): %s", r.Method, r.URL.Path, action, err.Error())
	}
	writeError(w, status, err.Error())
}

// decodeBody 解析 JSON 请求体；失败时已写好响应，返回 false。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError 出错也回 JSON —— 前端要能把服务端的话原样显示出来
// （比如"操作人不能为空"），而不是只看到一个状态码。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// clientIP 取直连来源 IP。与 internal/server 同一口径：**刻意不信任
// X-Forwarded-For / X-Real-IP** —— 那些头客户端可以伪造，记进审计等于假线索。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
