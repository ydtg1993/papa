// Package report 把 obs.Report 渲染成人类可读的 Markdown 与机读 JSON。
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
)

// VerdictLabel 把判定转成中文标签。
func VerdictLabel(v obs.Verdict) string {
	switch v {
	case obs.VerdictPass:
		return "通过"
	case obs.VerdictWarn:
		return "带风险通过"
	case obs.VerdictFail:
		return "不通过"
	default:
		return string(v)
	}
}

// Markdown 渲染人读报告。
func Markdown(rep *obs.Report) string {
	if rep == nil {
		return "# 运行报告\n\n（无数据）\n"
	}
	var b strings.Builder
	title := "运行报告"
	if len(rep.Run.RunIDs) > 0 {
		title += " · " + rep.Run.RunIDs[0]
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "**判定：%s**（%s）\n\n", VerdictLabel(rep.Verdict), rep.Verdict)
	fmt.Fprintf(&b, "- 窗口：%s ~ %s\n", fmtTime(rep.Run.From), fmtTime(rep.Run.To))
	fmt.Fprintf(&b, "- 运行：%s\n", strings.Join(rep.Run.RunIDs, ", "))
	fmt.Fprintf(&b, "- 事件：%d 条（文件 %d、seq 缺口 %d、坏行 %d、重复 %d）\n\n",
		rep.EventMeta.Count, rep.EventMeta.Files, rep.EventMeta.Gaps, rep.EventMeta.Unparsed, rep.EventMeta.Duplicates)

	if len(rep.Degraded) > 0 {
		b.WriteString("## 降级说明\n\n")
		for _, d := range rep.Degraded {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}

	if len(rep.Checks) > 0 {
		b.WriteString("## 判定项\n\n| 检查 | 级别 | 结果 | 说明 | 口径 |\n| --- | --- | --- | --- | --- |\n")
		for _, c := range rep.Checks {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n",
				c.ID, c.Severity, passText(c.Passed), c.Message, c.Detail)
		}
		b.WriteString("\n")
	}

	if len(rep.Problems) > 0 {
		b.WriteString("## 问题清单\n\n")
		for i, p := range rep.Problems {
			fmt.Fprintf(&b, "### %d. %s\n\n", i+1, p.Title)
			fmt.Fprintf(&b, "- 级别：%s", p.Severity)
			if p.Scope != "" {
				fmt.Fprintf(&b, " · 范围：%s", p.Scope)
			}
			if p.Count > 0 {
				fmt.Fprintf(&b, " · 次数：%d", p.Count)
			}
			b.WriteString("\n")
			for _, ev := range p.Evidence {
				fmt.Fprintf(&b, "- 证据（%s）：`%s`", ev.Type, ev.Ref)
				if ev.Excerpt != "" {
					fmt.Fprintf(&b, " — %s", oneLine(ev.Excerpt, 200))
				}
				b.WriteString("\n")
			}
			for _, h := range p.Hypotheses {
				fmt.Fprintf(&b, "- 假设（%s）：%s", h.Confidence, h.Text)
				if h.Verify != "" {
					fmt.Fprintf(&b, " · 验证：%s", h.Verify)
				}
				if h.FixHint != "" {
					fmt.Fprintf(&b, " · 修复方向：%s", h.FixHint)
				}
				b.WriteString("\n")
			}
			if p.Acceptance != "" {
				fmt.Fprintf(&b, "- 验收：%s\n", p.Acceptance)
			}
			b.WriteString("\n")
		}
	}

	for _, sec := range rep.Sections {
		fmt.Fprintf(&b, "## %s\n\n", sec.Title)
		if sec.Summary != "" {
			fmt.Fprintf(&b, "%s\n\n", sec.Summary)
		}
		for _, t := range sec.Tables {
			renderTable(&b, t)
		}
	}

	b.WriteString("---\n\n")
	b.WriteString("> 本报告只读生成：不含控制动作，不修改任何数据。\n")
	return b.String()
}

func renderTable(b *strings.Builder, t obs.Table) {
	if t.Title != "" {
		fmt.Fprintf(b, "### %s\n\n", t.Title)
	}
	if len(t.Columns) == 0 {
		return
	}
	fmt.Fprintf(b, "| %s |\n", strings.Join(t.Columns, " | "))
	fmt.Fprintf(b, "|%s\n", strings.Repeat(" --- |", len(t.Columns)))
	for _, row := range t.Rows {
		fmt.Fprintf(b, "| %s |\n", strings.Join(row, " | "))
	}
	b.WriteString("\n")
}

// JSON 渲染机读报告（缩进、稳定）。
func JSON(rep *obs.Report) ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// Write 把 report.md / report.json / ai-brief.md / ai-brief.json 写进 dir，返回写出的文件路径。
func Write(dir string, rep *obs.Report) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create report dir: %w", err)
	}
	mdPath := filepath.Join(dir, "report.md")
	if err := os.WriteFile(mdPath, []byte(Markdown(rep)), 0o644); err != nil {
		return nil, fmt.Errorf("write report.md: %w", err)
	}
	raw, err := JSON(rep)
	if err != nil {
		return nil, fmt.Errorf("encode report.json: %w", err)
	}
	jsonPath := filepath.Join(dir, "report.json")
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		return nil, fmt.Errorf("write report.json: %w", err)
	}
	aiMD := filepath.Join(dir, "ai-brief.md")
	if err := os.WriteFile(aiMD, []byte(AIBriefMarkdown(rep)), 0o644); err != nil {
		return nil, fmt.Errorf("write ai-brief.md: %w", err)
	}
	aiRaw, err := AIBriefJSON(rep)
	if err != nil {
		return nil, fmt.Errorf("encode ai-brief.json: %w", err)
	}
	aiJSON := filepath.Join(dir, "ai-brief.json")
	if err := os.WriteFile(aiJSON, aiRaw, 0o644); err != nil {
		return nil, fmt.Errorf("write ai-brief.json: %w", err)
	}
	return []string{mdPath, jsonPath, aiMD, aiJSON}, nil
}

// SortedRunIDs 给调用方拼目录名用：按字典序去重。
func SortedRunIDs(ids []string) []string {
	set := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || set[id] {
			continue
		}
		set[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// DirName 给报告产物目录选一个稳定名字：单 run 用 run_id，多 run 用窗口起点。
func DirName(run obs.RunMeta) string {
	if len(run.RunIDs) == 1 {
		return sanitizeDirName(run.RunIDs[0])
	}
	if !run.From.IsZero() {
		return "window-" + run.From.Format("20060102-150405")
	}
	return "window-" + time.Now().Format("20060102-150405")
}

func sanitizeDirName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			out = append(out, c)
		} else {
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "run"
	}
	return string(out)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

func passText(ok bool) string {
	if ok {
		return "✅ 通过"
	}
	return "❌ 未通过"
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
