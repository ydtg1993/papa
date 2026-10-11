package main

import "testing"

// orDash 只出现在 `papa upgrade` 的对比输出里（空值显示成 "-"）—— 一行工具函数，
// 但"空串被原样打出来"会让报告的每一行都看起来有值。
func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Fatalf("空串应当显示成 -，实得 %q", got)
	}
	if got := orDash("abc"); got != "abc" {
		t.Fatalf("非空原样返回，实得 %q", got)
	}
}
