package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeConfigRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")

	poolSize := 5
	maxIdle := Duration{Duration: 10 * time.Minute}
	timeout := Duration{Duration: 20 * time.Second}
	maxBody := int64(2048)
	rt := &RuntimeConfig{
		Browser: RuntimeBrowserConfig{
			PoolSize:    &poolSize,
			MaxIdleTime: &maxIdle,
			Headers:     map[string]string{"User-Agent": "custom-ua"},
		},
		HTML: RuntimeHTMLConfig{
			Timeout:     &timeout,
			MaxBodySize: &maxBody,
		},
	}

	if err := SaveRuntime(path, rt); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadRuntime(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Browser.PoolSize == nil || *loaded.Browser.PoolSize != 5 {
		t.Fatalf("pool_size mismatch: %+v", loaded.Browser.PoolSize)
	}
	if loaded.Browser.MaxIdleTime == nil || loaded.Browser.MaxIdleTime.Duration != 10*time.Minute {
		t.Fatalf("max_idle_time mismatch: %+v", loaded.Browser.MaxIdleTime)
	}
	if loaded.Browser.Headers["User-Agent"] != "custom-ua" {
		t.Fatalf("browser headers mismatch: %+v", loaded.Browser.Headers)
	}
	if loaded.HTML.Timeout == nil || loaded.HTML.Timeout.Duration != 20*time.Second {
		t.Fatalf("html timeout mismatch: %+v", loaded.HTML.Timeout)
	}
	if loaded.HTML.MaxBodySize == nil || *loaded.HTML.MaxBodySize != 2048 {
		t.Fatalf("html max_body_size mismatch: %+v", loaded.HTML.MaxBodySize)
	}
	// 未覆盖字段应为 nil
	if loaded.Browser.DirectSize != nil {
		t.Fatalf("direct_pool_size should be nil, got %+v", loaded.Browser.DirectSize)
	}
	if loaded.HTML.Headers != nil {
		t.Fatalf("html headers should be nil, got %+v", loaded.HTML.Headers)
	}
}

func TestRuntimeConfigHeaderCasePreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	data := []byte("browser:\n  headers:\n    User-Agent: custom-ua\n    Accept-Language: zh-CN\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rt, err := LoadRuntime(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rt.Browser.Headers["User-Agent"] != "custom-ua" || rt.Browser.Headers["Accept-Language"] != "zh-CN" {
		t.Fatalf("header case not preserved: %+v", rt.Browser.Headers)
	}
}

func TestLoadRuntimeMissing(t *testing.T) {
	rt, err := LoadRuntime(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if rt == nil || !rt.IsZero() {
		t.Fatalf("expected empty runtime config, got %+v", rt)
	}
}

func TestSaveRuntimeEmptyRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	poolSize := 3
	if err := SaveRuntime(path, &RuntimeConfig{Browser: RuntimeBrowserConfig{PoolSize: &poolSize}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}
	if err := SaveRuntime(path, &RuntimeConfig{}); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file removed after empty save, stat err=%v", err)
	}
}
