package crawler

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
)

func TestEngineRuntimeGetters(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Browser: config.BrowserConfig{
			PoolSize:    3,
			DirectSize:  1,
			MaxIdleTime: 5 * time.Minute,
			Headers:     map[string]string{"User-Agent": "base-ua", "X-Base": "1"},
		},
		HTML: config.HTMLConfig{
			Timeout:     15 * time.Second,
			MaxBodySize: 1024,
			Headers:     map[string]string{"User-Agent": "base-html-ua"},
		},
	}}
	e.runtime.Store(&config.RuntimeConfig{})

	// 无覆盖：回退基础配置
	if got := e.browserMaxIdle(); got != 5*time.Minute {
		t.Fatalf("browserMaxIdle = %v, want 5m", got)
	}

	// 覆盖层生效
	maxIdle := config.Duration{Duration: 10 * time.Minute}
	timeout := config.Duration{Duration: 30 * time.Second}
	maxBody := int64(4096)
	e.runtime.Store(&config.RuntimeConfig{
		Browser: config.RuntimeBrowserConfig{
			MaxIdleTime: &maxIdle,
			Headers:     map[string]string{"User-Agent": "overlay-ua", "X-Overlay": "2"},
		},
		HTML: config.RuntimeHTMLConfig{
			Timeout:     &timeout,
			MaxBodySize: &maxBody,
			Headers:     map[string]string{"X-Html-Overlay": "3"},
		},
	})

	if got := e.browserMaxIdle(); got != 10*time.Minute {
		t.Fatalf("browserMaxIdle = %v, want 10m", got)
	}

	// 请求头合并：内置默认 + 基础 + 覆盖（覆盖优先）
	headers := e.browserHeaders()
	if headers["X-Base"] != "1" {
		t.Fatalf("base header lost: %+v", headers)
	}
	if headers["X-Overlay"] != "2" {
		t.Fatalf("overlay header lost: %+v", headers)
	}
	if headers["User-Agent"] != "overlay-ua" {
		t.Fatalf("browser User-Agent = %q, want overlay-ua", headers["User-Agent"])
	}

	// HTML 客户端配置：UA 取自 html 侧（基础 html headers），timeout/maxBody 取覆盖
	cfg := e.htmlConfig()
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("html timeout = %v, want 30s", cfg.Timeout)
	}
	if cfg.MaxBodySize != 4096 {
		t.Fatalf("html maxBody = %d, want 4096", cfg.MaxBodySize)
	}
	if cfg.Headers["X-Html-Overlay"] != "3" {
		t.Fatalf("html overlay header lost: %+v", cfg.Headers)
	}
	if cfg.UserAgent != "base-html-ua" {
		t.Fatalf("html UA = %q, want base-html-ua", cfg.UserAgent)
	}
}

func TestApplyRuntimeConfigNilComponents(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	e.runtime.Store(&config.RuntimeConfig{})

	maxIdle := config.Duration{Duration: 2 * time.Minute}
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{
		Browser: config.RuntimeBrowserConfig{MaxIdleTime: &maxIdle},
	}); err != nil {
		t.Fatalf("ApplyRuntimeConfig: %v", err)
	}
	got := e.GetRuntimeConfig().Browser.MaxIdleTime
	if got == nil || got.Duration != 2*time.Minute {
		t.Fatalf("runtime max_idle_time = %+v, want 2m", got)
	}
}

// ApplyRuntimeConfig 接的是**增量**，不是整份覆盖层：只提交一个字段，不能把之前设的别的清掉。
// （PUT /api/config 每次都只带操作人改的那几个字段，整体替换会让"改一个字段"变成"重置其余全部"。）
func TestApplyRuntimeConfigMergesDelta(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	e.runtime.Store(&config.RuntimeConfig{})

	maxIdle := config.Duration{Duration: 10 * time.Minute}
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{
		Browser: config.RuntimeBrowserConfig{MaxIdleTime: &maxIdle},
	}); err != nil {
		t.Fatal(err)
	}

	timeout := config.Duration{Duration: 30 * time.Second}
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{
		HTML: config.RuntimeHTMLConfig{Timeout: &timeout},
	}); err != nil {
		t.Fatal(err)
	}

	rt := e.GetRuntimeConfig()
	if rt.Browser.MaxIdleTime == nil || rt.Browser.MaxIdleTime.Duration != 10*time.Minute {
		t.Fatalf("第二次提交不该清掉第一次设的 browser.max_idle_time：%+v", rt.Browser)
	}
	if rt.HTML.Timeout == nil || rt.HTML.Timeout.Duration != 30*time.Second {
		t.Fatalf("第二次提交的 html.timeout 应生效：%+v", rt.HTML)
	}
}
