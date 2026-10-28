package server

import (
	"strings"
	"testing"
)

// obs 报告页要能显示「未判定」、快照模式、引擎暂停与未通过的检查；
// 前端没有测试框架，只能把文案与字段名钉在静态文件里，防止某次重构把展示弄丢。
func TestMoJSObsReportCredibilityUI(t *testing.T) {
	raw, err := staticFS.ReadFile("static/mo.js")
	if err != nil {
		t.Fatalf("read mo.js: %v", err)
	}
	js := string(raw)
	for _, want := range []string{"'unknown'", "未判定", "快照", "引擎暂停", "fail_reason_top"} {
		if !strings.Contains(js, want) {
			t.Errorf("mo.js 缺少 %q（未知判定/模式/暂停/失败原因展示）", want)
		}
	}
}
