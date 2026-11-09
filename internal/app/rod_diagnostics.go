package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type rodStep struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Milliseconds int64  `json:"milliseconds"`
	Error        string `json:"error,omitempty"`
}
type rodReport struct {
	ID          string         `json:"id"`
	Time        time.Time      `json:"time"`
	Target      string         `json:"target"`
	OK          bool           `json:"ok"`
	Environment map[string]any `json:"environment"`
	Steps       []rodStep      `json:"steps"`
	BrowserLog  string         `json:"browser_log"`
	Hint        string         `json:"hint"`
}

var rodDiagnosticMu sync.Mutex
var diagnosticURL = regexp.MustCompile(`https?://[^\s<>"']+`)
var diagnosticSecret = regexp.MustCompile(`(?i)(token|password|authorization|cookie|secret|api[_-]?key)([=: ]+)[^\s,;]+`)

func redactRod(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	text = diagnosticURL.ReplaceAllStringFunc(text, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[URL]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	})
	text = diagnosticSecret.ReplaceAllString(text, "$1$2[REDACTED]")
	if len(text) > 16384 {
		text = text[:16384] + "\n[truncated]"
	}
	return text
}

func RodDiagnostics(app *App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !rodDiagnosticMu.TryLock() {
			http.Error(w, "已有浏览器诊断正在运行", 409)
			return
		}
		defer rodDiagnosticMu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		report := runRodDiagnostic(ctx, app)
		if app.Logger != nil && app.Logger.Sys != nil {
			data, _ := json.Marshal(report)
			app.Logger.Sys.Infof("rod diagnostics %s", data)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(report)
	}
}
func newRodReport(target string) rodReport {
	return rodReport{ID: uuid.NewString(), Time: time.Now(), Target: target, Environment: map[string]any{}, Steps: []rodStep{}}
}
func (r *rodReport) step(name string, secrets []string, fn func() error) bool {
	for _, s := range r.Steps {
		if s.Status == "failed" {
			r.Steps = append(r.Steps, rodStep{Name: name, Status: "skipped"})
			return false
		}
	}
	start := time.Now()
	err := fn()
	s := rodStep{Name: name, Status: "ok", Milliseconds: time.Since(start).Milliseconds()}
	if err != nil {
		s.Status = "failed"
		s.Error = redactRod(err.Error(), secrets)
		r.Hint = "请结合浏览器输出排查。EOF 表示连接中断，不能据此确认 OOM；Linux 可核对同一时间的 Chromium 退出及内核 OOM 日志。"
	}
	r.Steps = append(r.Steps, s)
	return err == nil
}
