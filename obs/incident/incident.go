// Package incident analyzes bounded, immutable circuit-breaker evidence.
package incident

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v3/pkg/redact"
)

const MaxSamples = 256

type Sample struct {
	At        time.Time `json:"at"`
	TaskID    int64     `json:"task_id"`
	Stage     string    `json:"stage"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Error     string    `json:"error"`
	Truncated bool      `json:"truncated"`
}

// Capture is frozen at trip time. All fields are diagnostic copies.
type Capture struct {
	ID        string        `json:"id"`
	Site      string        `json:"site"`
	At        time.Time     `json:"at"`
	From      time.Time     `json:"from"`
	ResumedAt time.Time     `json:"resumed_at"`
	Window    time.Duration `json:"window"`
	Threshold int           `json:"threshold"`
	Failures  int           `json:"failures"`
	Limited   bool          `json:"limited"`
	Samples   []Sample      `json:"-"`
}

type Group struct {
	Stage      string    `json:"stage"`
	Kind       string    `json:"kind"`
	Template   string    `json:"template"`
	Count      int       `json:"count"`
	Tasks      int       `json:"tasks"`
	Percent    float64   `json:"percent"`
	First      time.Time `json:"first"`
	Last       time.Time `json:"last"`
	Category   string    `json:"category"`
	Hypothesis string    `json:"hypothesis"`
	Verify     string    `json:"verify"`
	Examples   []Sample  `json:"examples"`
}

type Report struct {
	Capture
	Observed int      `json:"observed"`
	Complete bool     `json:"complete"`
	Summary  string   `json:"summary"`
	Notes    []string `json:"notes"`
	Groups   []Group  `json:"groups"`
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func Clean(s Sample) Sample {
	s.Truncated = s.Truncated || len(s.Error) > 1024 || len(s.URL) > 512 || len(s.Stage) > 128 || len(s.Kind) > 128
	// Never cut a credential in half before redaction (e.g. long URL userinfo).
	// Oversized input is omitted entirely to keep parsing work bounded.
	if len(s.Error) > 8192 {
		s.Error = "错误文本超过 8192 字节，未采集原文，请查看受控服务日志"
	}
	if len(s.URL) > 4096 {
		s.URL = "[URL 超出采集上限]"
	}
	s.Error = clip(redact.Text(s.Error), 1024)
	s.URL = clip(redact.URL(s.URL), 512)
	s.Stage = clip(redact.Text(s.Stage), 128)
	s.Kind = clip(redact.Text(s.Kind), 128)
	return s
}

var urls = regexp.MustCompile(`https?://[^\s<>"']+`)
var uuid = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
var taskID = regexp.MustCompile(`(?i)\b(task[_ -]?id|pid|request[_ -]?id)[=: ]+[a-z0-9-]+`)
var chromeStamp = regexp.MustCompile(`\[(?:\d+:){0,2}\d{4}/\d{6}(?:\.\d+)?`)
var chromePID = regexp.MustCompile(`\[(?:\d+:){1,3}(ERROR|WARNING|INFO|FATAL)`)
var isoStamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?\b`)

// Keep numeric status/error codes: HTTP 403 and HTTP 429 must never merge.
func Template(s string) string {
	s = urls.ReplaceAllString(s, "<URL>")
	s = uuid.ReplaceAllString(s, "<UUID>")
	s = taskID.ReplaceAllString(s, "$1=<ID>")
	s = chromeStamp.ReplaceAllString(s, "[<PID/TIME>")
	s = chromePID.ReplaceAllString(s, "[<PID>:$1")
	s = isoStamp.ReplaceAllString(s, "<TIME>")
	return strings.Join(strings.Fields(s), " ")
}

func Analyze(c Capture) Report {
	r := Report{Capture: c, Groups: []Group{}, Notes: []string{}}
	r.Samples = nil
	type entry struct {
		group Group
		tasks map[int64]bool
	}
	groups := map[string]*entry{}
	for _, raw := range c.Samples {
		if raw.At.Before(c.From) || raw.At.After(c.At) {
			continue
		}
		if r.Observed >= MaxSamples {
			r.Limited = true
			break
		}
		s := Clean(raw)
		r.Observed++
		if s.Truncated {
			r.Limited = true
		}
		if strings.TrimSpace(s.Error) == "" {
			r.Limited = true
		}
		tpl := Template(s.Error)
		key := s.Stage + "\x00" + s.Kind + "\x00" + tpl
		e := groups[key]
		if e == nil {
			category, hypothesis, verify := Classify(s.Kind, s.Error)
			e = &entry{group: Group{Stage: s.Stage, Kind: s.Kind, Template: tpl, First: s.At, Last: s.At, Category: category, Hypothesis: hypothesis, Verify: verify}, tasks: map[int64]bool{}}
			groups[key] = e
		}
		e.group.Count++
		if s.TaskID > 0 {
			e.tasks[s.TaskID] = true
		}
		if s.At.Before(e.group.First) {
			e.group.First = s.At
		}
		if s.At.After(e.group.Last) {
			e.group.Last = s.At
		}
		if len(e.group.Examples) < 3 {
			e.group.Examples = append(e.group.Examples, s)
		}
	}
	for _, e := range groups {
		e.group.Tasks = len(e.tasks)
		e.group.Percent = float64(e.group.Count) * 100 / float64(r.Observed)
		r.Groups = append(r.Groups, e.group)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		a, b := r.Groups[i], r.Groups[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Stage+"\x00"+a.Kind+"\x00"+a.Template < b.Stage+"\x00"+b.Kind+"\x00"+b.Template
	})
	r.Complete = !r.Limited && r.Observed == c.Failures && r.Observed > 0
	r.Notes = append(r.Notes, "占比以本次采样的终态失败数为分母，不是请求失败率；原因是待验证假设。", "计数器采用近似滑动窗口，边界可能与精确时间样本略有差异；未知任务编号不计入去重任务数。")
	if !r.Complete {
		r.Notes = append(r.Notes, "证据不完整：可能达到采样/文本上限、事件缺失或窗口边界存在差异，不能推断未采样部分。")
	}
	if len(r.Groups) == 0 {
		r.Summary = "没有可用失败样本，无法归因"
	} else {
		g := r.Groups[0]
		r.Summary = fmt.Sprintf("采样 %d 次终态失败；最大相似错误组为 %s / %s：%d 次（%.1f%%），涉及 %d 个已知任务", r.Observed, g.Stage, g.Category, g.Count, g.Percent, g.Tasks)
	}
	return r
}

// Text is plain text for the monitor/copy/download; UI must use textContent.
func Text(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "熔断分析 %s\n站点：%s\n触发：%s\n证据窗口：%s — %s\n配置窗口：%s；阈值：%d；触发计数：%d\n", r.ID, r.Site, r.At.Format(time.RFC3339), r.From.Format(time.RFC3339), r.At.Format(time.RFC3339), r.Window, r.Threshold, r.Failures)
	if !r.ResumedAt.IsZero() {
		fmt.Fprintf(&b, "上次整站恢复：%s；恢复后 %s 再次熔断\n", r.ResumedAt.Format(time.RFC3339), r.At.Sub(r.ResumedAt).Round(time.Millisecond))
	}
	fmt.Fprintf(&b, "\n%s\n证据完整：%t\n", r.Summary, r.Complete)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "说明：%s\n", n)
	}
	for i, g := range r.Groups {
		fmt.Fprintf(&b, "\n%d. %s / %s（%s）\n次数：%d，占采样 %.1f%%；已知任务：%d\n首次：%s；最近：%s\n错误特征：%s\n可能原因（未验证）：%s\n验证步骤：%s\n", i+1, g.Stage, g.Category, g.Kind, g.Count, g.Percent, g.Tasks, g.First.Format(time.RFC3339), g.Last.Format(time.RFC3339), g.Template, g.Hypothesis, g.Verify)
		for _, s := range g.Examples {
			fmt.Fprintf(&b, "样本任务 %d：%s\n脱敏错误原文：%s\n", s.TaskID, s.URL, s.Error)
		}
	}
	return b.String()
}
