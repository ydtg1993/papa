package engine

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
)

// 浏览器头与 HTML 客户端配置**只从 cfg 读**（运行期覆盖层已随热更一起取消：
// 改这些参数 = 改 config.yaml / 站点声明 + 重启）。
func TestBrowserAndHTMLConfigComeFromCfg(t *testing.T) {
	e := &Engine{cfg: &config.Config{
		Browser: config.BrowserConfig{
			MaxIdleTime: 5 * time.Minute,
			Headers:     map[string]string{"User-Agent": "base-ua", "X-Base": "1"},
		},
		HTML: config.HTMLConfig{
			Timeout:     15 * time.Second,
			MaxBodySize: 1024,
			Headers:     map[string]string{"User-Agent": "base-html-ua"},
		},
	}}

	if got := e.browserMaxIdle(); got != 5*time.Minute {
		t.Fatalf("browserMaxIdle = %v, want 5m", got)
	}
	if headers := e.browserHeaders(); headers["User-Agent"] != "base-ua" || headers["X-Base"] != "1" {
		t.Fatalf("browserHeaders = %+v", headers)
	}
	hc := e.htmlConfig()
	if hc.Timeout != 15*time.Second || hc.MaxBodySize != 1024 || hc.UserAgent != "base-html-ua" {
		t.Fatalf("htmlConfig = %+v", hc)
	}
	if _, ok := hc.Headers["User-Agent"]; ok {
		t.Fatal("User-Agent 该从 Headers 里摘出来单放（htmlfetch.Config 有它自己的字段）")
	}
}
