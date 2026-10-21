package server

import (
	"io/fs"
	"strings"
	"testing"
)

// 追踪抽屉对「捕获网络请求」失败现场的渲染：前端没有测试框架，只能把接线钉在静态文件里
// （同 TestBellPanelWired / TestSiteControlPanelWired）。
//
// 判据是 data 驱动（有 candidates / timeout_ms 才画），所以这里钉的是**键名**：
// 引擎写什么键、前端读什么键，两边写错一个就是静默不显示。
func TestTraceDrawerRendersCaptureCandidates(t *testing.T) {
	js, err := fs.ReadFile(staticFS, "static/trace.js")
	if err != nil {
		t.Fatalf("read trace.js: %v", err)
	}
	src := string(js)
	for _, want := range []string{
		"trace-candidates", // 候选请求的折叠块
		"candidates_total", // 「共 N 条」的总数
		"data.candidates",  // 样本数组
		"timeout_ms",       // 步骤头的「超时 30s」
		"elapsed_ms",       // 步骤头的「实等 30.2s」
		"fmtMillis",        // 毫秒格式化（不是 fmtDuration 的纳秒）
		"stepData",         // data 解析的唯一出口（archivedFiles 也走它）
	} {
		if !strings.Contains(src, want) {
			t.Errorf("trace.js 里缺少 %q —— 捕获失败现场在抽屉里会静默不显示", want)
		}
	}

	css, err := fs.ReadFile(staticFS, "static/mo.css")
	if err != nil {
		t.Fatalf("read mo.css: %v", err)
	}
	if !strings.Contains(string(css), ".trace-candidates") {
		t.Error("mo.css 里缺少 .trace-candidates 样式")
	}
}
