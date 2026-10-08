package core

import "testing"

// 级别名会原样进告警 JSON（webhook 那边按它路由），所以四个级别的拼写是契约。
func TestAlertLevelString(t *testing.T) {
	cases := map[AlertLevel]string{
		AlertInfo:     "info",
		AlertWarn:     "warn",
		AlertError:    "error",
		AlertCritical: "critical",
	}
	for level, want := range cases {
		if got := level.String(); got != want {
			t.Errorf("AlertLevel(%d).String() = %q, want %q", level, got, want)
		}
	}

	// 越界值给 unknown 而不是 panic 或空串：日志里得看得出"这是个没定义的级别"
	for _, bad := range []AlertLevel{-1, 99} {
		if got := bad.String(); got != "unknown" {
			t.Errorf("AlertLevel(%d).String() = %q, want unknown", bad, got)
		}
	}
	// 级别是有序的：critical 必须比 error 高（webhook 靠比较大小决定要不要 @全体）
	if !(AlertInfo < AlertWarn && AlertWarn < AlertError && AlertError < AlertCritical) {
		t.Fatal("级别的相对大小变了")
	}
}
