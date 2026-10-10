package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadConfig 覆盖 Load 的时长解析、DurationRange、header 大小写保留等关键路径，
// 防止回归（尤其 yaml.v3 替代 viper 后）。
func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := `
app:
  env: dev
log:
  dir: ./logs
db:
  max_idle_conns: 10
  max_open_conns: 100
browser:
  enable: true
  pool_size: 3
  max_idle_time: "5m"
  headers:
    User-Agent: "test-ua"
    Accept-Language: "zh-CN"
html:
  enable: true
  timeout: "15s"
  max_body_size: 10485760
  headers:
    User-Agent: "html-ua"
crawler:
  queue_watermark: 0.5
  drain_interval: "5s"
  stop_timeout: "8s"
  trace:
    enabled: true
    retention: "168h"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Browser.MaxIdleTime != 5*time.Minute {
		t.Fatalf("max_idle_time = %v, want 5m", cfg.Browser.MaxIdleTime)
	}
	if cfg.HTML.Timeout != 15*time.Second {
		t.Fatalf("html timeout = %v, want 15s", cfg.HTML.Timeout)
	}
	if cfg.HTML.MaxBodySize != 10485760 {
		t.Fatalf("max_body_size = %d", cfg.HTML.MaxBodySize)
	}
	// header 大小写保留
	if cfg.Browser.Headers["User-Agent"] != "test-ua" {
		t.Fatalf("browser headers 大小写丢失: %+v", cfg.Browser.Headers)
	}
	if cfg.HTML.Headers["User-Agent"] != "html-ua" {
		t.Fatalf("html headers 大小写丢失: %+v", cfg.HTML.Headers)
	}
	// 注意：阶段参数已搬进 Go 声明（`configs/sites/<站名>.go` 的 StageSpec），配置里不再有 crawler.stages 段；
	// 那套 "10s-30s" 区间的解析由 duration_test.go 直接覆盖 config.ParseDurationRange。
	// 三个治理队列同理，也不在这里了（搬到站点声明）—— 它们出现在 config.yaml 里会被拒绝启动，
	// 见 validate_test.go 的 TestMovedQueueSectionsPointToSiteDeclaration。
	if cfg.Crawler.QueueWatermark != 0.5 {
		t.Fatalf("queue_watermark = %v, want 0.5", cfg.Crawler.QueueWatermark)
	}
	if cfg.Crawler.DrainInterval != 5*time.Second {
		t.Fatalf("drain_interval = %v, want 5s", cfg.Crawler.DrainInterval)
	}
	// stop_timeout：配了就用配的；没配（0）回退默认 5s —— 存量业务项目的 config.yaml 里没有这个键
	if cfg.Crawler.StopTimeout != 8*time.Second {
		t.Fatalf("stop_timeout = %v, want 8s", cfg.Crawler.StopTimeout)
	}
	if got := (CrawlerConfig{}).StopTimeoutOrDefault(); got != 5*time.Second {
		t.Fatalf("空的 stop_timeout 应回退默认 5s，实得 %v", got)
	}
	if !cfg.Crawler.Trace.Enabled || cfg.Crawler.Trace.Retention != 168*time.Hour {
		t.Fatalf("crawler.trace = %+v, want enabled + retention 168h", cfg.Crawler.Trace)
	}
}

// db.log_level 写错了要在启动前报清楚，不能静默当默认值用。
func TestLoadPanicsOnBadSQLLogLevel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	// 三个必填键都得在：log.dir + 两个连接池键（理由见 validate_test.go 的 TestRequiredKeys）
	base := "log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n  log_level: "

	// 非法的级别名要在启动前炸掉（进校验层之后形态从「返回 error」变成「panic」）
	writeFile(t, p, base+"verbose")
	assertPanicNamesKey(t, "db.log_level", func() { Load(p) })

	// 合法的四种 + 留空都要能过
	for _, lvl := range []string{"", "silent", "error", "warn", "info"} {
		writeFile(t, p, base+lvl)
		if _, err := Load(p); err != nil {
			t.Fatalf("log_level=%q 应当合法：%v", lvl, err)
		}
	}
}

// SQL 日志级别：显式优先，没配按环境推。
func TestSQLLogLevelResolution(t *testing.T) {
	dev := &Config{}
	dev.App.Env = "dev"
	if got := dev.SQLLogLevel(); got != "info" {
		t.Fatalf("dev 默认 = %q, want info", got)
	}
	if dev.SQLHideParams() {
		t.Fatal("dev 不该隐参数值")
	}

	prod := &Config{}
	prod.App.Env = "prod"
	if got := prod.SQLLogLevel(); got != "warn" {
		t.Fatalf("prod 默认 = %q, want warn", got)
	}
	if !prod.SQLHideParams() {
		t.Fatal("非 dev 必须隐参数值")
	}

	prod.DB.LogLevel = "silent"
	if got := prod.SQLLogLevel(); got != "silent" {
		t.Fatalf("显式配了应当优先，实得 %q", got)
	}
}

// HTTP 超时：0 补默认，但 write 的 0 是"不限"—— 日志打包下载可能传很久，给它设上限等于掐断在途下载。
func TestHTTPTimeoutDefaults(t *testing.T) {
	var s ServerConfig
	readHeader, read, write, idle, shutdown := s.HTTPTimeouts()
	if readHeader != 10*time.Second || read != 30*time.Second || idle != 60*time.Second || shutdown != 10*time.Second {
		t.Fatalf("默认值不对：%v/%v/%v/%v", readHeader, read, idle, shutdown)
	}
	if write != 0 {
		t.Fatalf("write 的 0 必须保持「不限」，实得 %v", write)
	}

	s = ServerConfig{
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 4 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 6 * time.Second, ShutdownTimeout: 7 * time.Second,
	}
	readHeader, read, write, idle, shutdown = s.HTTPTimeouts()
	if readHeader != 3*time.Second || read != 4*time.Second || write != 5*time.Second ||
		idle != 6*time.Second || shutdown != 7*time.Second {
		t.Fatalf("显式配了应当原样生效：%v/%v/%v/%v/%v", readHeader, read, write, idle, shutdown)
	}
}
