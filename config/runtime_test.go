package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeConfigRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")

	maxIdle := Duration{Duration: 10 * time.Minute}
	timeout := Duration{Duration: 20 * time.Second}
	maxBody := int64(2048)
	rt := &RuntimeConfig{
		Browser: RuntimeBrowserConfig{
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
	if loaded.HTML.Headers != nil {
		t.Fatalf("html headers should be nil, got %+v", loaded.HTML.Headers)
	}
}

// 老 runtime.yaml 里遗留的 pool_size 不属于热更字段，读取时应被忽略而不是报错。
func TestLoadRuntimeIgnoresLegacyPoolSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.yaml")
	data := []byte("browser:\n  pool_size: 9\n  direct_pool_size: 2\n  max_idle_time: 3m\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rt, err := LoadRuntime(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if rt.Browser.MaxIdleTime == nil || rt.Browser.MaxIdleTime.Duration != 3*time.Minute {
		t.Fatalf("max_idle_time should still load: %+v", rt.Browser.MaxIdleTime)
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
	maxIdle := Duration{Duration: time.Minute}
	if err := SaveRuntime(path, &RuntimeConfig{Browser: RuntimeBrowserConfig{MaxIdleTime: &maxIdle}}); err != nil {
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

// PUT /api/config 的语义是「改我提到的字段」：没提到的必须保持原样。
// 回归点：原来是整体替换，只提交 html.timeout 会把之前设的 browser.headers、
// 各队列的 interval 全清掉 —— 操作人从响应上完全看不出来。
func TestRuntimeConfigMergeKeepsUntouchedFields(t *testing.T) {
	maxIdle := Duration{Duration: 10 * time.Minute}
	interval := Duration{Duration: 5 * time.Minute}
	cur := &RuntimeConfig{
		Browser: RuntimeBrowserConfig{
			MaxIdleTime: &maxIdle,
			Headers:     map[string]string{"User-Agent": "custom-ua"},
		},
		ErrorQueue: RuntimeErrorQueueConfig{Interval: &interval},
	}

	// 只提 html.timeout
	timeout := Duration{Duration: 30 * time.Second}
	got := cur.Merge(&RuntimeConfig{HTML: RuntimeHTMLConfig{Timeout: &timeout}})

	if got.HTML.Timeout == nil || got.HTML.Timeout.Duration != 30*time.Second {
		t.Fatalf("提到的字段应被覆盖：%+v", got.HTML.Timeout)
	}
	if got.Browser.MaxIdleTime == nil || got.Browser.MaxIdleTime.Duration != 10*time.Minute {
		t.Fatalf("没提到的 browser.max_idle_time 必须原样保留：%+v", got.Browser.MaxIdleTime)
	}
	if got.Browser.Headers["User-Agent"] != "custom-ua" {
		t.Fatalf("没提到的 browser.headers 必须原样保留：%+v", got.Browser.Headers)
	}
	if got.ErrorQueue.Interval == nil || got.ErrorQueue.Interval.Duration != 5*time.Minute {
		t.Fatalf("没提到的队列 interval 必须原样保留：%+v", got.ErrorQueue.Interval)
	}

	// 合并是纯函数：接收者不能被改
	if cur.HTML.Timeout != nil {
		t.Fatalf("Merge 不该改动接收者：%+v", cur.HTML.Timeout)
	}
}

// 映射用 nil 与空 map 区分：nil = 没提（保留），{} = 显式把这组覆盖清空。
func TestRuntimeConfigMergeMapSemantics(t *testing.T) {
	cur := &RuntimeConfig{Browser: RuntimeBrowserConfig{Headers: map[string]string{"A": "1"}}}

	// 没提 headers → 保留
	keep := cur.Merge(&RuntimeConfig{})
	if keep.Browser.Headers["A"] != "1" {
		t.Fatalf("没提 headers 时应保留：%+v", keep.Browser.Headers)
	}

	// 显式给 {} → 清空这组覆盖
	cleared := cur.Merge(&RuntimeConfig{Browser: RuntimeBrowserConfig{Headers: map[string]string{}}})
	if len(cleared.Browser.Headers) != 0 {
		t.Fatalf("显式给空 map 应清空这组覆盖：%+v", cleared.Browser.Headers)
	}

	// 给别的键 → 整组替换（不是往旧 map 里 merge）
	replaced := cur.Merge(&RuntimeConfig{Browser: RuntimeBrowserConfig{Headers: map[string]string{"B": "2"}}})
	if _, ok := replaced.Browser.Headers["A"]; ok || replaced.Browser.Headers["B"] != "2" {
		t.Fatalf("给了 headers 应当整组替换：%+v", replaced.Browser.Headers)
	}
}

// 空 body（{}）不改变任何东西 —— 最自然的读法是"什么都没提"。
func TestRuntimeConfigMergeEmptyIsNoop(t *testing.T) {
	maxIdle := Duration{Duration: time.Minute}
	cur := &RuntimeConfig{Browser: RuntimeBrowserConfig{MaxIdleTime: &maxIdle}}
	got := cur.Merge(&RuntimeConfig{})
	if got.Browser.MaxIdleTime == nil || got.Browser.MaxIdleTime.Duration != time.Minute {
		t.Fatalf("空增量不应改变覆盖层：%+v", got)
	}
}
