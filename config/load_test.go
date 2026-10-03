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
  trace:
    enabled: true
    retention: "168h"
  stages:
    catalog:
      worker_count: 2
      queue_size: 10
      delay: "10s-30s"
      retry:
        max_attempts: 3
        backoff: "30s"
error_queue:
  interval: "10m"
  batch_size: 500
recover_queue:
  enabled: true
  worker_count: 2
  interval: "10m"
  timeout: "6h"
  batch_size: 500
repeat_queue:
  enabled: true
  worker_count: 3
  interval: "5m"
  batch_size: 200
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
	// DurationRange 与 time.Duration
	stage := cfg.Crawler.Stages["catalog"]
	if stage.Delay.Min != 10*time.Second || stage.Delay.Max != 30*time.Second {
		t.Fatalf("delay = %+v, want 10s-30s", stage.Delay)
	}
	if stage.Retry.Backoff != 30*time.Second {
		t.Fatalf("backoff = %v, want 30s", stage.Retry.Backoff)
	}
	if cfg.ErrorQueue.Interval != 10*time.Minute {
		t.Fatalf("error_queue.interval = %v, want 10m", cfg.ErrorQueue.Interval)
	}
	if cfg.ErrorQueue.BatchSize != 500 {
		t.Fatalf("error_queue.batch_size = %v, want 500", cfg.ErrorQueue.BatchSize)
	}
	if !cfg.RecoverQueue.Enabled || cfg.RecoverQueue.WorkerCount != 2 {
		t.Fatalf("recover_queue = %+v, want enabled + worker_count 2", cfg.RecoverQueue)
	}
	if cfg.RecoverQueue.Interval != 10*time.Minute || cfg.RecoverQueue.Timeout != 6*time.Hour {
		t.Fatalf("recover_queue interval/timeout = %v/%v, want 10m/6h",
			cfg.RecoverQueue.Interval, cfg.RecoverQueue.Timeout)
	}
	if cfg.RecoverQueue.BatchSize != 500 {
		t.Fatalf("recover_queue.batch_size = %v, want 500", cfg.RecoverQueue.BatchSize)
	}
	if !cfg.RepeatQueue.Enabled || cfg.RepeatQueue.WorkerCount != 3 {
		t.Fatalf("repeat_queue = %+v, want enabled + worker_count 3", cfg.RepeatQueue)
	}
	if cfg.RepeatQueue.Interval != 5*time.Minute || cfg.RepeatQueue.BatchSize != 200 {
		t.Fatalf("repeat_queue interval/batch_size = %v/%v, want 5m/200",
			cfg.RepeatQueue.Interval, cfg.RepeatQueue.BatchSize)
	}
	if cfg.Crawler.QueueWatermark != 0.5 {
		t.Fatalf("queue_watermark = %v, want 0.5", cfg.Crawler.QueueWatermark)
	}
	if cfg.Crawler.DrainInterval != 5*time.Second {
		t.Fatalf("drain_interval = %v, want 5s", cfg.Crawler.DrainInterval)
	}
	if !cfg.Crawler.Trace.Enabled || cfg.Crawler.Trace.Retention != 168*time.Hour {
		t.Fatalf("crawler.trace = %+v, want enabled + retention 168h", cfg.Crawler.Trace)
	}
}
