package config

import (
	"testing"
	"time"
)

/* ---------- DurationRange ---------- */

// 零值/空区间视为"固定"（Max <= Min），Random 返回 Min —— 这样 stage 没配 delay 时
// Random() 就是 0，worker 不会凭空睡一觉。
func TestDurationRangeZeroValueIsFixed(t *testing.T) {
	var zero DurationRange
	if !zero.Fixed() {
		t.Fatal("零值应当是固定值")
	}
	if got := zero.Random(); got != 0 {
		t.Fatalf("零值 Random() = %v, want 0", got)
	}

	// Max < Min 的畸形配置也不该 panic，同样按固定值处理
	weird := DurationRange{Min: 10 * time.Second, Max: time.Second}
	if !weird.Fixed() || weird.Random() != 10*time.Second {
		t.Fatalf("Max < Min 时应当按 Min 处理，实得 %v", weird.Random())
	}
}

// 只有固定/空区间、末尾带空白、非法段数等边界。
func TestParseDurationRangeEdges(t *testing.T) {
	if got, err := ParseDurationRange(" 30s "); err != nil || got.Min != 30*time.Second {
		t.Fatalf("两端空白应被裁掉：%+v %v", got, err)
	}
	if got, err := ParseDurationRange("1s - 2s"); err != nil || got.Min != time.Second || got.Max != 2*time.Second {
		t.Fatalf("区间两端的空白应被裁掉：%+v %v", got, err)
	}
	for _, bad := range []string{"", "   ", "1s-2s-3s", "notaduration", "1s-notaduration", "notaduration-2s"} {
		if _, err := ParseDurationRange(bad); err == nil {
			t.Errorf("ParseDurationRange(%q) 应当报错", bad)
		}
	}
}

// 非法文本走 UnmarshalText 时要报错，而不是把零值悄悄当成"固定 0"。
func TestDurationRangeUnmarshalTextRejectsBadInput(t *testing.T) {
	var d DurationRange
	if err := d.UnmarshalText([]byte("nonsense")); err == nil {
		t.Fatal("非法文本应当报错")
	}
	if d != (DurationRange{}) {
		t.Fatalf("解析失败不该改动接收者：%+v", d)
	}
}

/* ---------- Merge 的 nil 容忍 ---------- */

/* ---------- 各默认值 helper ---------- */

// 熔断窗口：没配（或配成非正）时补默认 5m。
//
// 阈值**没有对应的 helper** —— 它没有默认值：enabled=true 却没给正阈值是非法配置，
// 由 breaker.New 直接 panic（见 internal/breaker 的 TestEnabledWithoutThresholdPanics）。
func TestBreakerWindowOrDefault(t *testing.T) {
	var zero BreakerConfig
	if got := zero.WindowOrDefault(); got != 5*time.Minute {
		t.Fatalf("默认窗口 = %v, want 5m", got)
	}
	if got := (BreakerConfig{Window: -time.Second}).WindowOrDefault(); got != 5*time.Minute {
		t.Fatalf("负窗口应回退默认：%v", got)
	}
	if got := (BreakerConfig{Window: time.Minute}).WindowOrDefault(); got != time.Minute {
		t.Fatalf("显式配置不该被覆盖：%v", got)
	}
}

// 关停排空上限：0/负补成 5s。它和 server.shutdown_timeout 是两件事，各有各的默认值。
func TestStopTimeoutOrDefault(t *testing.T) {
	if got := (CrawlerConfig{}).StopTimeoutOrDefault(); got != 5*time.Second {
		t.Fatalf("默认 = %v, want 5s", got)
	}
	if got := (CrawlerConfig{StopTimeout: -time.Second}).StopTimeoutOrDefault(); got != 5*time.Second {
		t.Fatalf("负值应回退默认：%v", got)
	}
	if got := (CrawlerConfig{StopTimeout: time.Second}).StopTimeoutOrDefault(); got != time.Second {
		t.Fatalf("显式值不该被覆盖：%v", got)
	}
}

// HTTP 超时：四个正数项补默认，**写超时不补** —— 0 就是"不限"，
// 日志打包下载可能传很久，给它设上限等于把在途下载掐断。
func TestHTTPTimeoutsDefaultsAndWriteException(t *testing.T) {
	rh, r, w, idle, sd := (ServerConfig{}).HTTPTimeouts()
	if rh != 10*time.Second || r != 30*time.Second || idle != 60*time.Second || sd != 10*time.Second {
		t.Fatalf("默认值不对：%v %v %v %v", rh, r, idle, sd)
	}
	if w != 0 {
		t.Fatalf("写超时 0 表示不限，不该被补成默认：%v", w)
	}

	cfg := ServerConfig{
		ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second,
		IdleTimeout: 3 * time.Second, ShutdownTimeout: 4 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	rh, r, w, idle, sd = cfg.HTTPTimeouts()
	if rh != time.Second || r != 2*time.Second || idle != 3*time.Second || sd != 4*time.Second || w != 5*time.Second {
		t.Fatalf("显式值被改动了：%v %v %v %v %v", rh, r, w, idle, sd)
	}

	// 负值同样回退默认
	neg := ServerConfig{ReadHeaderTimeout: -1, ReadTimeout: -1, IdleTimeout: -1, ShutdownTimeout: -1}
	rh, r, _, idle, sd = neg.HTTPTimeouts()
	if rh != 10*time.Second || r != 30*time.Second || idle != 60*time.Second || sd != 10*time.Second {
		t.Fatalf("负值应回退默认：%v %v %v %v", rh, r, idle, sd)
	}
}

// SQL 日志：非 dev 一律隐掉参数值 —— 即便只打慢查询，那行 SQL 也带着参数值。
func TestSQLHideParamsFollowsEnv(t *testing.T) {
	if (&Config{App: AppConfig{Env: "dev"}}).SQLHideParams() {
		t.Fatal("dev 环境要显示参数值（本地调试要用）")
	}
	for _, env := range []string{"", "prod", "ol", "staging"} {
		if !(&Config{App: AppConfig{Env: env}}).SQLHideParams() {
			t.Fatalf("env=%q 应当隐掉参数值", env)
		}
	}
}
