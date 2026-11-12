package browserdiagnostics

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type RodStep struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Milliseconds int64  `json:"milliseconds"`
	Error        string `json:"error,omitempty"`
}
type RodReport struct {
	ID          string         `json:"id"`
	Time        time.Time      `json:"time"`
	Target      string         `json:"target"`
	OK          bool           `json:"ok"`
	Environment map[string]any `json:"environment"`
	Steps       []RodStep      `json:"steps"`
	BrowserLog  string         `json:"browser_log"`
	Hint        string         `json:"hint"`
}

var diagnosticURL = regexp.MustCompile(`https?://[^\s<>"']+`)
var diagnosticSecret = regexp.MustCompile(`(?i)(token|password|authorization|cookie|secret|api[_-]?key)([=: ]+)[^\s,;]+`)

func Redact(text string, secrets []string) string {
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

func newRodReport(target string) RodReport {
	return RodReport{ID: uuid.NewString(), Time: time.Now(), Target: target, Environment: map[string]any{}, Steps: []RodStep{}}
}
func (r *RodReport) step(name string, secrets []string, fn func() error) bool {
	for _, s := range r.Steps {
		if s.Status == "failed" {
			r.Steps = append(r.Steps, RodStep{Name: name, Status: "skipped"})
			return false
		}
	}
	start := time.Now()
	err := fn()
	s := RodStep{Name: name, Status: "ok", Milliseconds: time.Since(start).Milliseconds()}
	if err != nil {
		s.Status = "failed"
		s.Error = Redact(err.Error(), secrets)
		r.Hint = "请结合浏览器输出排查。EOF 表示连接中断，不能据此确认 OOM；Linux 可核对同一时间的 Chromium 退出及内核 OOM 日志。"
	}
	r.Steps = append(r.Steps, s)
	return err == nil
}
