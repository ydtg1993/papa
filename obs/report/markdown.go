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
	case obs.VerdictUnknown:
		return "未判定"
	default:
		return string(v)
	}
}

// modeText 把判定模式转成带解释的短标签；空模式来自旧版报告（没有 mode 字段）。
func modeText(m obs.Mode) string {
	switch m {
	case obs.ModeSnapshot:
		return "snapshot（快照，不代表验收结论）"
	case obs.ModeAcceptance:
		return "acceptance（验收）"
	default:
		return "-（历史报告）"
	}
}

// Markdown 渲染人读报告。
func Markdown(rep *obs.Report) string {
	if rep == nil {
		return "# 运行报告\n\n（无数据）\n"
	}
	var b strings.Builder
	title := "运行报告"
	// 单 run 且窗口在同一天时才敢在标题里挂 run_id：跨天/多 run 的窗口不代表
	// 那个 run 发生在窗口内，标题只给日期与 run 数。
	switch {
	case len(rep.Run.RunIDs) == 1 && !rep.Run.From.IsZero() &&
		rep.Run.From.Format("2006-01-02") == rep.Run.To.Format("2006-01-02"):
		title += " · " + rep.Run.RunIDs[0]
	case !rep.Run.From.IsZero():
		title += fmt.Sprintf(" · %s · %d 个 run", rep.Run.From.Format("2006-01-02"), len(rep.Run.RunIDs))
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "**判定：%s**（%s）\n\n", VerdictLabel(rep.Verdict), rep.Verdict)
	fmt.Fprintf(&b, "- 模式：%s\n", modeText(rep.Mode))
	if rep.VerdictReason != "" {
		fmt.Fprintf(&b, "- 判定原因：%s\n", rep.VerdictReason)
	}
	fmt.Fprintf(&b, "- 生成时间：%s（最近一次更新）\n", fmtTime(rep.GeneratedAt))
	// 自证块：报告要能被独立复核 —— 数字取自哪一刻的库、由什么版本+配置+阈值算出、事件覆盖到哪。
	// 零值字段直接跳过（历史报告没有 inputs，不能渲染成空行或 0001 年）。
	if !rep.Inputs.SnapshotAt.IsZero() {
		fmt.Fprintf(&b, "- DB 快照时刻：%s（状态层数字取这一时刻的全库现状）\n", fmtTime(rep.Inputs.SnapshotAt))
	}
	if rep.Inputs.PapaVersion != "" {
		fmt.Fprintf(&b, "- 版本：%s", rep.Inputs.PapaVersion)
		if rep.Inputs.PapaCommit != "" {
			fmt.Fprintf(&b, " (commit %s)", rep.Inputs.PapaCommit)
		}
		b.WriteString("\n")
	}
	if rep.Inputs.ConfigFile != "" {
		fmt.Fprintf(&b, "- 配置：%s\n", rep.Inputs.ConfigFile)
	}
	if len(rep.Inputs.Thresholds) > 0 {
		fmt.Fprintf(&b, "- 阈值来源：%s（%s）\n", rep.Inputs.ThresholdsSource, thresholdText(rep.Inputs.Thresholds))
	}
	if !rep.Inputs.Events.FirstTS.IsZero() {
		fmt.Fprintf(&b, "- 事件覆盖：%s ~ %s，最大事件间隔 %s\n",
			fmtTime(rep.Inputs.Events.FirstTS), fmtTime(rep.Inputs.Events.LastTS), rep.Inputs.Events.MaxGap)
	}
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
				c.ID, c.Severity, checkResultText(c), checkMessageText(c), c.Detail)
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
	if err := writeFileAtomic(mdPath, []byte(Markdown(rep))); err != nil {
		return nil, fmt.Errorf("write report.md: %w", err)
	}
	raw, err := JSON(rep)
	if err != nil {
		return nil, fmt.Errorf("encode report.json: %w", err)
	}
	jsonPath := filepath.Join(dir, "report.json")
	if err := writeFileAtomic(jsonPath, raw); err != nil {
		return nil, fmt.Errorf("write report.json: %w", err)
	}
	aiMD := filepath.Join(dir, "ai-brief.md")
	if err := writeFileAtomic(aiMD, []byte(AIBriefMarkdown(rep))); err != nil {
		return nil, fmt.Errorf("write ai-brief.md: %w", err)
	}
	aiRaw, err := AIBriefJSON(rep)
	if err != nil {
		return nil, fmt.Errorf("encode ai-brief.json: %w", err)
	}
	aiJSON := filepath.Join(dir, "ai-brief.json")
	if err := writeFileAtomic(aiJSON, aiRaw); err != nil {
		return nil, fmt.Errorf("write ai-brief.json: %w", err)
	}
	return []string{mdPath, jsonPath, aiMD, aiJSON}, nil
}

// writeFileAtomic 写临时文件再 rename：后台可能在读，不能让它读到半份。
// 每份产物各自独立：中途失败时旧文件保持原样，defer 清掉本次的临时文件。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是无效路径，失败时清掉
	// CreateTemp 默认 0600；旧实现是 os.WriteFile(..., 0o644)，产物可能要跨用户/被采集脚本读，
	// 这里保持 0644（Windows 上 chmod 基本是空操作，不影响）。
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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

// thresholdText 把阈值 map 渲染成稳定的 "k=v, k2=v2"：按 key 排序，
// 避免 map 迭代顺序让同一份报告的头部每次都不一样。
func thresholdText(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

func passText(ok bool) string {
	if ok {
		return "✅ 通过"
	}
	return "❌ 未通过"
}

// checkResultText 是判定项表的结果列：不适用必须与"未通过"区分开，
// 否则暂停/无阶段时会把不适用读成红。
//
// info 级检查表达的是「状态」（活跃阶段池/引擎空闲/恢复中/贯通进度），不是通过或失败：
// 它们的 Passed 语义是「该状态是否成立」，渲染成 ✅/❌ 会出现「引擎空闲 ❌ 未通过」
// 这种结果列与说明列互相打脸的行。所以 info 非 skipped 统一显示「ℹ️ 状态」，
// 具体状态看说明列；判定只看 fail/warn（见 obs.VerdictOf）。
func checkResultText(c obs.Check) string {
	if c.Skipped {
		if c.SkipReason != "" {
			return "➖ 不适用（" + c.SkipReason + "）"
		}
		return "➖ 不适用"
	}
	if c.Severity == obs.SeverityInfo {
		return "ℹ️ 状态"
	}
	return passText(c.Passed)
}

// checkMessageText 给不适用且没有说明的判定项兜底，避免表格出现空单元格。
func checkMessageText(c obs.Check) string {
	if c.Skipped && strings.TrimSpace(c.Message) == "" {
		return "-"
	}
	return c.Message
}

func oneLine(s string, n int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
