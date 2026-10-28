package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newObsReportFixture 造一个报告目录：
//   - window-20261028-153000：完整四份产物，report.json 判定 warn，最新；
//   - run-old：完整四份产物，report.json 判定 pass，最旧；
//   - run-partial：只有 report.md（历史写入中断/手工放的半份报告）；
//   - not-a-report/：无关目录；根目录还有一个散落文件，两者都不该出现在列表里。
func newObsReportFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	// 完整报告两份：文件内容用不同标记，读接口能区分读到的是哪一份。
	// report.json 带上 mode/generated_at/engine/checks：列表摘要要用这几项。
	for _, run := range []struct{ dir, verdict, mode, marker, generatedAt string }{
		{"run-old", "pass", "acceptance", "OLD", "2026-10-28T10:00:00+08:00"},
		{"window-20261028-153000", "warn", "snapshot", "NEW", "2026-10-28T15:30:00+08:00"},
	} {
		write(run.dir+"/report.md", "# "+run.marker+" 运行报告\n")
		write(run.dir+"/report.json", `{"schema_version":1,"verdict":"`+run.verdict+
			`","mode":"`+run.mode+`","generated_at":"`+run.generatedAt+
			`","engine":{"source":"engine","active_pools":0,"sites":[{"key":"a","paused":true}]},`+
			`"checks":[{"id":"stuck_processing","severity":"fail","passed":false}]}`+"\n")
		write(run.dir+"/ai-brief.md", "# "+run.marker+" AI 简报\n")
		write(run.dir+"/ai-brief.json", `{"verdict":"`+run.verdict+`"}`+"\n")
	}
	write("run-partial/report.md", "# 半份报告\n")
	write("not-a-report/notes.txt", "不是报告目录\n")
	write("README.md", "根目录散落文件\n")

	now := time.Now()
	touch := func(dir string, at time.Time) {
		t.Helper()
		if err := os.Chtimes(filepath.Join(root, dir), at, at); err != nil {
			t.Fatalf("chtimes %s: %v", dir, err)
		}
	}
	touch("run-old", now.Add(-48*time.Hour))
	touch("run-partial", now.Add(-time.Hour))
	touch("window-20261028-153000", now.Add(-time.Minute))
	return root
}

// 报告列表：只认含已知产物的子目录，按生成时间倒序，带 report.json 的判定/模式/暂停/失败原因。
func TestObsReportList(t *testing.T) {
	root := newObsReportFixture(t)
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ReportDir: root})
	rr := serve(m, http.MethodGet, "/api/obs/reports", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)
	if got := body["report_dir"]; got != root {
		t.Errorf("report_dir = %v, want %s", got, root)
	}
	runs, ok := body["runs"].([]any)
	if !ok {
		t.Fatalf("runs missing or wrong type: %T", body["runs"])
	}
	if len(runs) != 3 {
		t.Fatalf("runs 数量 = %d, want 3（无关目录与散落文件不该出现）：%v", len(runs), runs)
	}
	first := runs[0].(map[string]any)
	if got := first["name"]; got != "window-20261028-153000" {
		t.Errorf("第一份报告 = %v, want 最新的 window-20261028-153000", got)
	}
	if got := first["verdict"]; got != "warn" {
		t.Errorf("最新报告的 verdict = %v, want warn", got)
	}
	if got := first["mode"]; got != "snapshot" {
		t.Errorf("mode = %v, want snapshot", got)
	}
	if got := first["engine_paused"]; got != true {
		t.Errorf("engine_paused = %v, want true", got)
	}
	if got := first["generated_at"]; got == nil {
		t.Error("列表应返回 generated_at")
	}
	if files := first["files"].([]any); len(files) != 4 {
		t.Errorf("完整报告文件数 = %d, want 4", len(files))
	}
	// 按 generated_at 倒序：run-old（10:00）在只有 report.md 的 run-partial 之前，
	// 即使 run-partial 的目录 mtime 更新。
	second := runs[1].(map[string]any)
	if got := second["name"]; got != "run-old" {
		t.Errorf("第二份报告 = %v, want run-old（按 generated_at 倒序）", got)
	}
	third := runs[2].(map[string]any)
	if got := third["name"]; got != "run-partial" {
		t.Errorf("第三份报告 = %v, want run-partial（没有 generated_at 的排最后）", got)
	}
	if files := third["files"].([]any); len(files) != 1 {
		t.Errorf("半份报告文件数 = %d, want 1", len(files))
	}
	if _, has := third["verdict"]; has {
		t.Error("没有 report.json 的报告不该有 verdict 字段")
	}
}

// 排序优先用 report.json 的 generated_at，不再只看目录 mtime：
// 目录 mtime 与 generated_at 相反时，新报告必须排前面。
func TestObsReportListSortsByGeneratedAt(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("new-mtime-but-old-report/report.json",
		`{"verdict":"pass","generated_at":"2026-10-28T10:00:00+08:00"}`)
	write("old-mtime-but-new-report/report.json",
		`{"verdict":"warn","generated_at":"2026-10-28T15:30:00+08:00"}`)

	now := time.Now()
	// mtime 与 generated_at 故意相反：mtime 新的那份报告反而是旧的。
	_ = os.Chtimes(filepath.Join(root, "new-mtime-but-old-report"), now, now)
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(filepath.Join(root, "old-mtime-but-new-report"), old, old)

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ReportDir: root})
	rr := serve(m, http.MethodGet, "/api/obs/reports", nil)
	body := decodeJSON(t, rr)
	runs := body["runs"].([]any)
	if got := runs[0].(map[string]any)["name"]; got != "old-mtime-but-new-report" {
		t.Fatalf("应按 generated_at 倒序，第一份 = %v", got)
	}
}

// 报告目录没配置/不存在：回空列表而不是 500 —— 后台页面要能画空态。
func TestObsReportListEmptyOrMissingDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"没配置", ""},
		{"目录不存在", filepath.Join(t.TempDir(), "nope")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ReportDir: tc.dir})
			rr := serve(m, http.MethodGet, "/api/obs/reports", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
			}
			body := decodeJSON(t, rr)
			runs, ok := body["runs"].([]any)
			if !ok || len(runs) != 0 {
				t.Fatalf("runs = %v, want 空数组", body["runs"])
			}
		})
	}
}

// 读单个产物：原文 + text/plain；download=1 时带 attachment 头。
func TestObsReportFileReadAndDownload(t *testing.T) {
	root := newObsReportFixture(t)
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ReportDir: root})

	rr := serve(m, http.MethodGet, "/api/obs/report?run=run-old&file=report.md", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); !strings.Contains(got, "OLD 运行报告") {
		t.Errorf("body = %q, want run-old 的 report.md 原文", got)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if nosniff := rr.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", nosniff)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("普通查看不该带 Content-Disposition，实得 %q", cd)
	}

	rr = serve(m, http.MethodGet, "/api/obs/report?run=run-old&file=ai-brief.json&download=1", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("download status = %d, body=%s", rr.Code, rr.Body.String())
	}
	wantCD := `attachment; filename="run-old-ai-brief.json"`
	if cd := rr.Header().Get("Content-Disposition"); cd != wantCD {
		t.Errorf("Content-Disposition = %q, want %q", cd, wantCD)
	}
}

// 下载路径是**从请求里来的**：越界、未知文件名、不存在的报告都要挡在读取之前。
func TestObsReportFileRejectsBadInput(t *testing.T) {
	root := newObsReportFixture(t)
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ReportDir: root})
	for _, tc := range []struct {
		name string
		url  string
		want int
	}{
		{"run 越界", "/api/obs/report?run=..&file=report.md", http.StatusBadRequest},
		{"run 带分隔符", "/api/obs/report?run=..%2Frun-old&file=report.md", http.StatusBadRequest},
		{"file 越界", "/api/obs/report?run=run-old&file=..%2Freport.md", http.StatusBadRequest},
		{"file 不在四份产物里", "/api/obs/report?run=run-old&file=notes.txt", http.StatusBadRequest},
		{"缺 file", "/api/obs/report?run=run-old", http.StatusBadRequest},
		{"缺 run", "/api/obs/report?file=report.md", http.StatusBadRequest},
		{"run 不存在", "/api/obs/report?run=nope&file=report.md", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := serve(m, http.MethodGet, tc.url, nil)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d, body=%s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

// 没配置报告目录时读接口回 404（不是空 200）：页面里的「下载」不该静默拿到空文件。
func TestObsReportFileWithoutReportDir(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{})
	rr := serve(m, http.MethodGet, "/api/obs/report?run=run-old&file=report.md", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rr.Code, rr.Body.String())
	}
}

// 超大文件直接拒绝：报告是给人工看的文本，超过上限说明路径/文件不对，不该整份读进内存。
func TestObsReportFileTooLarge(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "big-run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(runDir, "report.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(int64(obsReportMaxFileBytes) + 1); err != nil {
		_ = f.Close()
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()

	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ReportDir: root})
	rr := serve(m, http.MethodGet, "/api/obs/report?run=big-run&file=report.json", nil)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", rr.Code, rr.Body.String())
	}
}

// 「obs报告」页接线：模板导航在「设置」上面、模块壳存在；mo.js 的列表/Tab/查看/下载都在；
// 前端没有测试框架，只能把接线与样式类钉在静态文件里，防止某次重构把页面弄成白板。
func TestObsReportPageWired(t *testing.T) {
	tpl, err := readTemplateFile(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`data-module="obs"`, `id="mod-obs"`, `id="obs-runs"`, `id="obs-tabs"`, `id="obs-content"`, `downloadObsReport()`} {
		if !strings.Contains(tpl, want) {
			t.Errorf("template.html 里缺少 %q", want)
		}
	}
	// 菜单顺序：访问令牌 → obs报告 → 设置。
	nav := tpl
	posTokens := strings.Index(nav, `data-module="tokens"`)
	posObs := strings.Index(nav, `data-module="obs"`)
	posSettings := strings.Index(nav, `data-module="settings"`)
	if posObs < 0 || posSettings < 0 || posTokens < 0 {
		t.Fatalf("导航按钮缺失：tokens=%d obs=%d settings=%d", posTokens, posObs, posSettings)
	}
	if !(posObs < posSettings) {
		t.Error("「obs报告」菜单必须排在「设置」上面")
	}

	js, err := readStaticFile(t, "static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`obs: 'obs报告'`,
		"function fetchObsReports(",
		"'/api/obs/reports'",
		"function renderObsReports(",
		"function openObsReport(",
		"function obsVerdict(",
		"data-obs-file",
		"function downloadObsReport(",
		"'/api/obs/report?run='",
		"pre.textContent = await resp.text()",
		"if (name === 'obs') { fetchObsReports(); }",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("mo.js 里缺少 %q —— obs 报告页会静默失效", want)
		}
	}

	css, err := readStaticFile(t, "static/mo.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".obs-toolbar", ".obs-tabs", ".obs-tab.active", ".obs-download", ".obs-content"} {
		if !strings.Contains(css, want) {
			t.Errorf("mo.css 里缺少 %q", want)
		}
	}
}

// readTemplateFile / readStaticFile 是这两个静态断言用的小工具（读 embed FS）。
func readTemplateFile(t *testing.T) (string, error) {
	t.Helper()
	b, err := templateFS.ReadFile("template.html")
	return string(b), err
}

func readStaticFile(t *testing.T, name string) (string, error) {
	t.Helper()
	b, err := staticFS.ReadFile(name)
	return string(b), err
}
