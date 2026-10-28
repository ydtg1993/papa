package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

/* ---------- obs 段与 log.level：解码 / 默认值 / 枚举 ---------- */

// 老配置里既没有 log.level 也没有 obs 段 —— 必须照样能起来（向后兼容），
// 而且 Load 返回时默认值已经补齐：消费方（internal/app）读到的永远是生效值，
// 不用各自再写一遍 "空串该当哪个默认值"。
func TestObsAndLogDefaultsWhenAbsent(t *testing.T) {
	cfg := loadString(t, baseConfig)

	if cfg.Obs.Events.Enabled {
		t.Fatal("obs.events.enabled 默认必须 false —— 不写这段 = 不产任何观测文件")
	}
	if got := cfg.Obs.Events.File; got != "events.jsonl" {
		t.Fatalf("obs.events.file 默认 = %q, want events.jsonl", got)
	}
	if got := cfg.Obs.Events.Buffer; got != 8192 {
		t.Fatalf("obs.events.buffer 默认 = %d, want 8192", got)
	}
	if got := cfg.Obs.Events.SnapshotInterval; got != 0 {
		t.Fatalf("obs.events.snapshot_interval 默认 = %v, want 0（默认不发快照）", got)
	}
	if cfg.Obs.Report.OnExit {
		t.Fatal("obs.report.on_exit 默认必须 false（P1 只占位）")
	}
	if got := cfg.Obs.Report.Dir; got != "./reports" {
		t.Fatalf("obs.report.dir 默认 = %q, want ./reports", got)
	}
	if got := cfg.Log.Level; got != "info" {
		t.Fatalf("log.level 默认 = %q, want info", got)
	}
}

// 写了空值 / 0 / 负数与没写等价：默认值归一在 Validate 里做，是配置生效值的唯一出口。
func TestObsZeroValuesNormalized(t *testing.T) {
	cfg := loadString(t, baseConfig+`obs:
  events:
    enabled: true
    file: "   "
    buffer: -3
    snapshot_interval: "-5s"
  report:
    dir: ""
`)

	if got := cfg.Obs.Events.File; got != "events.jsonl" {
		t.Fatalf("空白 events.file 应归一为 events.jsonl，实得 %q", got)
	}
	if got := cfg.Obs.Events.Buffer; got != 8192 {
		t.Fatalf("负数 buffer 应归一为 8192，实得 %d", got)
	}
	if got := cfg.Obs.Events.SnapshotInterval; got != 0 {
		t.Fatalf("负 snapshot_interval 应归一为 0（不发），实得 %v", got)
	}
	if got := cfg.Obs.Report.Dir; got != "./reports" {
		t.Fatalf("空白 report.dir 应归一为 ./reports，实得 %q", got)
	}
}

// 显式开启：所有键都必须原样落到结构体上（含 snapshot_interval 的时长解析）。
// 这也钉住"开了就按写的值走"——默认值只补空位，不覆盖显式配置。
func TestObsConfigExplicitValues(t *testing.T) {
	cfg := loadString(t, `log:
  dir: ./logs
  level: debug
db:
  max_idle_conns: 10
  max_open_conns: 100
obs:
  events:
    enabled: true
    file: run-events.jsonl
    buffer: 256
    snapshot_interval: 30s
  report:
    on_exit: true
    dir: ./reports/dev
`)

	if !cfg.Obs.Events.Enabled {
		t.Fatal("obs.events.enabled 显式 true 没解析到")
	}
	if got := cfg.Obs.Events.File; got != "run-events.jsonl" {
		t.Fatalf("obs.events.file = %q, want run-events.jsonl", got)
	}
	if got := cfg.Obs.Events.Buffer; got != 256 {
		t.Fatalf("obs.events.buffer = %d, want 256", got)
	}
	if got := cfg.Obs.Events.SnapshotInterval; got != 30*time.Second {
		t.Fatalf("obs.events.snapshot_interval = %v, want 30s", got)
	}
	if !cfg.Obs.Report.OnExit {
		t.Fatal("obs.report.on_exit 显式 true 没解析到")
	}
	if got := cfg.Obs.Report.Dir; got != "./reports/dev" {
		t.Fatalf("obs.report.dir = %q, want ./reports/dev", got)
	}
	if got := cfg.Log.Level; got != "debug" {
		t.Fatalf("log.level = %q, want debug", got)
	}
}

// logLevelConfig 造一份最小合法配置，只把 log.level 换成 level。
// level 传 "" 时写成引号空串（YAML 的裸空值解出来是 null，测的就不是"写了空串"了）。
func logLevelConfig(level string) string {
	if level == "" {
		level = `""`
	}
	return "log:\n  dir: ./logs\n  level: " + level +
		"\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n"
}

// log.level 的三态：没写 → info（上面的默认值用例已钉）；写了合法 → 原样保留；
// 写了未知 → 启动即 panic，并且报错要点名键与全部合法值（只说"配置不对"等于没报）。
func TestLogLevelValidation(t *testing.T) {
	for _, lvl := range validLogLevels {
		t.Run("valid/"+lvl, func(t *testing.T) {
			cfg := loadString(t, logLevelConfig(lvl))
			if cfg.Log.Level != lvl {
				t.Fatalf("log.level = %q, want %q（合法值不该被改写）", cfg.Log.Level, lvl)
			}
		})
	}

	// 写了空串与没写等价
	if got := loadString(t, logLevelConfig("")).Log.Level; got != "info" {
		t.Fatalf("空 log.level 应归一为 info，实得 %q", got)
	}

	for _, bad := range []string{"verbose", "trace", "INFO", "warn2"} {
		t.Run("invalid/"+bad, func(t *testing.T) {
			msg := panicOn(t, logLevelConfig(bad))
			want := append([]string{"log.level", bad}, validLogLevels...)
			for _, w := range want {
				if !strings.Contains(msg, w) {
					t.Fatalf("panic 信息里缺 %q，实得：\n%s", w, msg)
				}
			}
		})
	}
}

// obs 段同样走"未知键一律拒绝启动"的老规则：拼错 bufffer 不能静默忽略 ——
// 这正是"配了个名字却没生效"里最贵的一类。
func TestObsUnknownKeyRejected(t *testing.T) {
	msg := panicOn(t, baseConfig+"obs:\n  events:\n    enabled: true\n    bufffer: 1024\n")
	for _, want := range []string{"未知配置键", "obs.events.bufffer"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("panic 信息里缺 %q，实得：\n%s", want, msg)
		}
	}
}

// 新字段必须能被 validKeys 反射出来 —— "是不是想写 X" 的建议与文档键集合都靠它。
func TestObsKeysAreRegistered(t *testing.T) {
	valid := validKeys()
	for _, want := range []string{
		"log.level",
		"obs.events.enabled",
		"obs.events.file",
		"obs.events.buffer",
		"obs.events.snapshot_interval",
		"obs.report.on_exit",
		"obs.report.dir",
	} {
		if !slices.Contains(valid, want) {
			t.Errorf("validKeys() 缺 %q", want)
		}
	}
}
