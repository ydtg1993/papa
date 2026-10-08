package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"
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

/* ---------- Duration（运行期覆盖层用） ---------- */

type durationHolder struct {
	D Duration `yaml:"d" json:"d"`
}

// YAML/JSON 里都写成人类可读的时长字符串 ——
// runtime.yaml 是运维手改的，写成纳秒整数没法维护。
func TestDurationRoundTrips(t *testing.T) {
	var y durationHolder
	if err := yaml.Unmarshal([]byte("d: 90s\n"), &y); err != nil {
		t.Fatalf("yaml 解析 = %v", err)
	}
	if y.D.Duration != 90*time.Second {
		t.Fatalf("yaml 值 = %v, want 90s", y.D.Duration)
	}
	out, err := yaml.Marshal(y)
	if err != nil {
		t.Fatalf("yaml 序列化 = %v", err)
	}
	// 注意 Go 的 Duration.String() 会规范化：90s 写成 1m30s（仍能原样解析回来）
	if !strings.Contains(string(out), "1m30s") {
		t.Fatalf("yaml 应写回 1m30s：%s", out)
	}

	var j durationHolder
	if err := json.Unmarshal([]byte(`{"d":"2m"}`), &j); err != nil {
		t.Fatalf("json 解析 = %v", err)
	}
	if j.D.Duration != 2*time.Minute {
		t.Fatalf("json 值 = %v, want 2m", j.D.Duration)
	}
	jb, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("json 序列化 = %v", err)
	}
	// 同样会被规范化成 2m0s
	if string(jb) != `{"d":"2m0s"}` {
		t.Fatalf("json 应写回字符串：%s", jb)
	}
}

// 非法时长在解析阶段就报错：热更配置写错了要在 PUT 那一刻被拒，而不是生效成 0。
func TestDurationRejectsBadInput(t *testing.T) {
	var y durationHolder
	if err := yaml.Unmarshal([]byte("d: nonsense\n"), &y); err == nil {
		t.Fatal("非法 yaml 时长应当报错")
	}

	var j durationHolder
	if err := json.Unmarshal([]byte(`{"d":"nonsense"}`), &j); err == nil {
		t.Fatal("非法 json 时长应当报错")
	}
	if err := json.Unmarshal([]byte(`{"d":123}`), &j); err == nil {
		t.Fatal("数字形式应当报错（只认 \"10s\" 这种字符串）")
	}
}

/* ---------- 各覆盖项的 IsZero ---------- */

// IsZero 决定 SaveRuntime 落不落盘、omitempty 出不出字段。
// 逐项空 vs 逐项非空都要对：漏一个就会在 runtime.yaml 里留下一个空壳分组。
func TestRuntimeSubConfigsIsZero(t *testing.T) {
	zero := true
	n := 5
	var n64 int64 = 5
	d := Duration{Duration: time.Second}

	cases := []struct {
		name string
		zero func() bool
		full func() bool
	}{
		{
			"browser",
			func() bool { return RuntimeBrowserConfig{}.IsZero() },
			func() bool {
				return RuntimeBrowserConfig{MaxIdleTime: &d, Headers: map[string]string{"a": "b"}}.IsZero()
			},
		},
		{
			"html",
			func() bool { return RuntimeHTMLConfig{}.IsZero() },
			func() bool { return RuntimeHTMLConfig{Timeout: &d, MaxBodySize: &n64}.IsZero() },
		},
		{
			"error_queue",
			func() bool { return RuntimeErrorQueueConfig{}.IsZero() },
			func() bool { return RuntimeErrorQueueConfig{Enabled: &zero, MaxRetry: &n}.IsZero() },
		},
		{
			"recover_queue",
			func() bool { return RuntimeRecoverQueueConfig{}.IsZero() },
			func() bool { return RuntimeRecoverQueueConfig{Enabled: &zero, BatchSize: &n}.IsZero() },
		},
		{
			"repeat_queue",
			func() bool { return RuntimeRepeatQueueConfig{}.IsZero() },
			func() bool { return RuntimeRepeatQueueConfig{WorkerCount: &n, Interval: &d}.IsZero() },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.zero() {
				t.Error("零值应当 IsZero")
			}
			if c.full() {
				t.Error("有覆盖项时不该 IsZero")
			}
		})
	}

	// 顶层：所有分组都空才算空；nil 也算空
	if !(&RuntimeConfig{}).IsZero() {
		t.Error("空覆盖层应当 IsZero")
	}
	var nilRT *RuntimeConfig
	if !nilRT.IsZero() {
		t.Error("nil 应当 IsZero")
	}
	full := &RuntimeConfig{HTML: RuntimeHTMLConfig{Timeout: &d}}
	if full.IsZero() {
		t.Error("有任一分组非空就不该 IsZero")
	}
}

/* ---------- Merge 的 nil 容忍 ---------- */

// Merge 对 nil 接收者 / nil 入参都按空处理：ApplyRuntimeConfig 可能拿到 nil。
func TestRuntimeConfigMergeNilSafe(t *testing.T) {
	var nilRT *RuntimeConfig
	d := Duration{Duration: time.Minute}

	got := nilRT.Merge(&RuntimeConfig{HTML: RuntimeHTMLConfig{Timeout: &d}})
	if got == nil || got.HTML.Timeout == nil {
		t.Fatalf("nil 接收者应被当作空覆盖层：%+v", got)
	}

	base := &RuntimeConfig{HTML: RuntimeHTMLConfig{Timeout: &d}}
	got = base.Merge(nil)
	if got == nil || got.HTML.Timeout == nil {
		t.Fatalf("nil 入参应被当作没提到任何字段：%+v", got)
	}
}

// Merge 不修改接收者，也不修改入参：调用方（引擎）持有的是只读覆盖层。
func TestRuntimeConfigMergeDoesNotMutate(t *testing.T) {
	d1 := Duration{Duration: time.Minute}
	d2 := Duration{Duration: 2 * time.Minute}
	cur := &RuntimeConfig{HTML: RuntimeHTMLConfig{Timeout: &d1}}
	next := &RuntimeConfig{Browser: RuntimeBrowserConfig{MaxIdleTime: &d2}}

	merged := cur.Merge(next)

	if cur.Browser.MaxIdleTime != nil {
		t.Fatal("Merge 改动了接收者")
	}
	if next.HTML.Timeout != nil {
		t.Fatal("Merge 改动了入参")
	}
	if merged.HTML.Timeout == nil || merged.HTML.Timeout.Duration != time.Minute {
		t.Fatalf("合并结果丢了 HTML 覆盖：%+v", merged.HTML)
	}
	if merged.Browser.MaxIdleTime == nil || merged.Browser.MaxIdleTime.Duration != 2*time.Minute {
		t.Fatalf("合并结果丢了 browser 覆盖：%+v", merged.Browser)
	}
}

/* ---------- 各默认值 helper ---------- */

// 熔断：窗口/阈值没配（或配成非正）时补默认值。
// 阈值 <= 0 在 breaker 里被解释成"只统计不熔断"，所以这里的默认值尤其要紧。
func TestBreakerConfigOrDefaults(t *testing.T) {
	var zero BreakerConfig
	if got := zero.WindowOrDefault(); got != 5*time.Minute {
		t.Fatalf("默认窗口 = %v, want 5m", got)
	}
	if got := zero.ThresholdOrDefault(); got != 50 {
		t.Fatalf("默认阈值 = %d, want 50", got)
	}

	neg := BreakerConfig{Window: -time.Second, Threshold: -1}
	if got := neg.WindowOrDefault(); got != 5*time.Minute {
		t.Fatalf("负窗口应回退默认：%v", got)
	}
	if got := neg.ThresholdOrDefault(); got != 50 {
		t.Fatalf("负阈值应回退默认：%d", got)
	}

	set := BreakerConfig{Window: time.Minute, Threshold: 3}
	if set.WindowOrDefault() != time.Minute || set.ThresholdOrDefault() != 3 {
		t.Fatalf("显式配置不该被覆盖：%+v", set)
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
