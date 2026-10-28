// obs 报告页的后端：把 obs.report.dir 下的历史运行报告列出来，并按需读取其中一份产物。
//
// 报告目录由 `papa report`、定时任务（obs.report.interval）或进程退出（obs.report.on_exit）写入，
// 每个运行一份目录，目录里固定四份产物：
// report.md / report.json / ai-brief.md / ai-brief.json（见 obs/report.Write）。
// 后端只做两件事：列目录（顺带流式读 report.json 的几个摘要字段：判定/模式/生成时间/引擎暂停/未通过检查）
// 和把单个文件原样当文本读出来 —— 不整体解析、不渲染、不改写。
// 报告内容里会有日志与 URL（不可信文本），前端按纯文本展示，下载走 attachment。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// obsReportFiles 是报告目录里的四份固定产物（与 obs/report.Write 的输出一致）。
var obsReportFiles = []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"}

// obsReportMaxRuns 报告列表最多回这么多份（按生成时间倒序）。历史报告通常一天一份，
// 100 份够看几个月；再多前端也翻不动，而且每份都要 stat 四个文件。
const obsReportMaxRuns = 100

// obsReportMaxFileBytes 单份产物的读取/下载上限。报告是给人看的文本，正常在几百 KB；
// 超过这个数多半是报告目录被换成了别的东西，直接拒绝比把进程内存打满好。
const obsReportMaxFileBytes = 16 << 20

// obsReportSummaryMaxBytes 流式读 report.json 摘要时的文件上限。报告可能几 MB
// （带日志/URL），超过这个数说明路径多半不对，不读也罢 —— 列表照常显示，只是没有摘要。
const obsReportSummaryMaxBytes = 8 << 20

// obsReportFileInfo 一份产物在列表里的样子。
type obsReportFileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// obsReportRunInfo 一个运行目录在列表里的样子。
type obsReportRunInfo struct {
	Name         string              `json:"name"`                      // 目录名（papa report 用 run_id / 窗口起点命名）
	Verdict      string              `json:"verdict,omitempty"`         // report.json 里的 verdict（pass/warn/fail/unknown）；读不到就空
	Mode         string              `json:"mode,omitempty"`            // report.json 里的 mode（snapshot/acceptance）
	EnginePaused bool                `json:"engine_paused,omitempty"`   // 报告里任一 site 处于暂停
	GeneratedAt  *time.Time          `json:"generated_at,omitempty"`    // 指针：没有该字段的旧报告不出 0001 年
	FailReasons  []string            `json:"fail_reason_top,omitempty"` // 最多 3 条未通过的 fail/warn 检查 ID
	ModTime      time.Time           `json:"mod_time"`
	Files        []obsReportFileInfo `json:"files"`
}

// obsReportListHandler 列出报告目录下的运行目录（GET /api/obs/reports）。
//
// 只认「目录里至少有一份已知产物」的子目录：报告目录里可能混着手工放的文件/无关目录，
// 那些不该出现在后台。目录不存在/没配置时回空列表（不是 500）—— 后台页面要能画空态。
func (s *Monitor) obsReportListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	dir := s.cfg.ReportDir
	runs := []obsReportRunInfo{}
	if dir != "" {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				run := obsReportRunInfo{Name: e.Name(), ModTime: info.ModTime(), Files: []obsReportFileInfo{}}
				for _, name := range obsReportFiles {
					fi, err := os.Stat(filepath.Join(dir, e.Name(), name))
					if err != nil || fi.IsDir() {
						continue
					}
					run.Files = append(run.Files, obsReportFileInfo{Name: name, Size: fi.Size(), ModTime: fi.ModTime()})
				}
				if len(run.Files) == 0 {
					continue // 不是报告目录（没有一份已知产物）
				}
				summary := readObsReportSummary(filepath.Join(dir, e.Name(), "report.json"))
				run.Verdict = summary.Verdict
				run.Mode = summary.Mode
				run.EnginePaused = summary.paused()
				if !summary.GeneratedAt.IsZero() {
					at := summary.GeneratedAt
					run.GeneratedAt = &at
				}
				run.FailReasons = summary.failReasons()
				runs = append(runs, run)
			}
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		// 生成时间优先（有 generated_at 的在前、按时间倒序），再退回目录 mtime。
		gi, gj := generatedAt(runs[i].GeneratedAt), generatedAt(runs[j].GeneratedAt)
		if !gi.Equal(gj) {
			if gi.IsZero() {
				return false
			}
			if gj.IsZero() {
				return true
			}
			return gi.After(gj)
		}
		if !runs[i].ModTime.Equal(runs[j].ModTime) {
			return runs[i].ModTime.After(runs[j].ModTime)
		}
		return runs[i].Name > runs[j].Name // 同一时刻按目录名倒序，保证顺序稳定
	})
	if len(runs) > obsReportMaxRuns {
		runs = runs[:obsReportMaxRuns]
	}
	writeJSON(w, map[string]any{"report_dir": dir, "runs": runs})
}

// obsReportFileHandler 读取/下载一份报告产物（GET /api/obs/report?run=<目录名>&file=<文件名>）。
//
// 默认回 text/plain 原文（前端放进 <pre> 看）；`download=1` 时带 attachment 头，浏览器直接存盘。
func (s *Monitor) obsReportFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ReportDir == "" {
		http.Error(w, "obs 报告未配置（obs.report.dir）", http.StatusNotFound)
		return
	}
	run := r.URL.Query().Get("run")
	file := r.URL.Query().Get("file")
	abs, err := resolveObsReportFile(s.cfg.ReportDir, run, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		http.Error(w, "报告文件不存在（可能已被清理）", http.StatusNotFound)
		return
	}
	if fi.Size() > obsReportMaxFileBytes {
		http.Error(w, "报告文件过大，请到服务器上查看", http.StatusRequestEntityTooLarge)
		return
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		http.Error(w, "报告文件读取失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// 报告里有日志/URL 等不可信文本：明确按纯文本读，别让浏览器按内容猜类型。
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+obsReportDownloadName(run, file)+`"`)
	}
	_, _ = w.Write(b)
}

// resolveObsReportFile 把 run/file 两个**请求参数**解析成报告目录下的绝对路径；越界一律拒绝。
//
// 参数是不可信输入：只接受单段名字（不含路径分隔符、不是 . / ..、不带盘符），文件名还必须在
// 四份已知产物里；最后再用 filepath.Rel 确认结果确实落在报告根目录之下。
func resolveObsReportFile(root, run, file string) (string, error) {
	run = strings.TrimSpace(run)
	file = strings.TrimSpace(file)
	if !validObsReportName(run) || !validObsReportName(file) {
		return "", fmt.Errorf("run / file 参数非法")
	}
	if !obsReportKnownFile(file) {
		return "", fmt.Errorf("未知的报告文件 %q", file)
	}
	abs := filepath.Join(root, run, file)
	inside, err := filepath.Rel(root, abs)
	if err != nil || filepath.IsAbs(inside) || inside == ".." ||
		strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("run / file 参数越界")
	}
	return abs, nil
}

// validObsReportName 判断一个参数是不是「单段文件/目录名」。
func validObsReportName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsRune(s, 0) {
		return false
	}
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	if strings.Contains(s, ":") { // Windows 盘符 / 备用数据流
		return false
	}
	return filepath.Base(s) == s
}

// obsReportKnownFile 文件必须在四份已知产物里（报告目录可能被手工塞入别的东西）。
func obsReportKnownFile(name string) bool {
	for _, f := range obsReportFiles {
		if f == name {
			return true
		}
	}
	return false
}

// obsReportSummary 是列表需要的 report.json 摘要，只声明关心的字段：
// json.Decoder 会跳过其余内容，内存不随报告体积（日志/URL/checks 详情）增长。
type obsReportSummary struct {
	Verdict     string    `json:"verdict"`
	Mode        string    `json:"mode"`
	GeneratedAt time.Time `json:"generated_at"`
	Engine      struct {
		ActivePools int `json:"active_pools"`
		Sites       []struct {
			Paused bool `json:"paused"`
		} `json:"sites"`
	} `json:"engine"`
	Checks []struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		Passed   bool   `json:"passed"`
		Skipped  bool   `json:"skipped"`
	} `json:"checks"`
}

// readObsReportSummary 从 report.json 里取列表摘要；文件不在/坏了/太大都当零值，不影响列表。
func readObsReportSummary(path string) obsReportSummary {
	var out obsReportSummary
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() > obsReportSummaryMaxBytes {
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&out); err != nil {
		return obsReportSummary{}
	}
	return out
}

// paused 报告里是否有任一 site 处于暂停（引擎暂停时判定会降级，页面要有提示）。
func (s obsReportSummary) paused() bool {
	for _, site := range s.Engine.Sites {
		if site.Paused {
			return true
		}
	}
	return false
}

// failReasons 取最多 3 条未通过的 fail/warn 检查 ID；跳过的不算失败。
func (s obsReportSummary) failReasons() []string {
	out := make([]string, 0, 3)
	for _, c := range s.Checks {
		if c.Passed || c.Skipped || (c.Severity != "fail" && c.Severity != "warn") {
			continue
		}
		out = append(out, c.ID)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// generatedAt 把可空时间摊平成可比较的值；零值代表这份报告没有 generated_at。
func generatedAt(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// obsReportDownloadName 拼下载文件名（run-file），并去掉会破坏 Content-Disposition 的字符。
func obsReportDownloadName(run, file string) string {
	return strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "").Replace(run + "-" + file)
}
