package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newLivePool 起一个只含一个真浏览器的池。捕获相关用例必须真起 Chromium：
// 事件时序（导航前订阅、请求在 load 前后发出）是假对象模拟不出来的。
func newLivePool(t *testing.T) *Pool {
	t.Helper()
	pool, err := NewPool(PoolConfig{
		Size:      1,
		Headless:  true,
		NoSandbox: true,
		Leakless:  false,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// capturePageServer 返回一个页面：页面里的 fetch 请求由 html 决定；.m3u8 统一返回播放列表。
func capturePageServer(t *testing.T, html string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\n"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 捕获命中：返回首条匹配请求，查询串（auth_key 这类签名）必须原样保留。
func TestCaptureRequestReturnsFirstMatch(t *testing.T) {
	srv := capturePageServer(t, `<html><body><script>
fetch('/api/health').catch(function(){});
setTimeout(function(){ fetch('/media/a.m3u8?auth_key=xyz&v=2'); }, 50);
</script></body></html>`)
	pool := newLivePool(t)

	var res *CaptureResult
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		var cerr error
		res, cerr = b.CaptureRequest(context.Background(), srv.URL+"/page", CaptureOptions{
			Match: func(r CapturedRequest) bool {
				return strings.HasSuffix(r.URL, ".m3u8?auth_key=xyz&v=2")
			},
			CaptureTimeout: 10 * time.Second,
			Page:           PageOptions{Timeout: 10 * time.Second},
		})
		return cerr
	})
	if err != nil {
		t.Fatalf("CaptureRequest: %v", err)
	}
	if res == nil || res.Request == nil {
		t.Fatalf("没有命中：%+v", res)
	}
	if !strings.Contains(res.Request.URL, "auth_key=xyz&v=2") {
		t.Fatalf("查询串被改坏了：%s", res.Request.URL)
	}
	if res.Request.Method != "GET" {
		t.Fatalf("method = %q, want GET", res.Request.Method)
	}
	if res.HTML != "" {
		t.Fatalf("命中路径不该读 HTML，实得 %d 字节", len(res.HTML))
	}
}

// 监听在导航前挂上：页面加载第一时间发出的请求也要能截到。
func TestCaptureRequestBeforeNavigationListener(t *testing.T) {
	srv := capturePageServer(t, `<html><body><script>
fetch('/media/early.m3u8');
</script></body></html>`)
	pool := newLivePool(t)

	var res *CaptureResult
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		var cerr error
		res, cerr = b.CaptureRequest(context.Background(), srv.URL+"/page", CaptureOptions{
			Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, "early.m3u8") },
			CaptureTimeout: 10 * time.Second,
		})
		return cerr
	})
	if err != nil {
		t.Fatalf("CaptureRequest: %v", err)
	}
	if res == nil || res.Request == nil || !strings.HasSuffix(res.Request.URL, "early.m3u8") {
		t.Fatalf("早期请求没有截到：%+v", res)
	}
}

// 超时失败要留现场：页面 HTML（验证码/404 长什么样）+ 未命中请求样本 + 总数。
func TestCaptureRequestTimeoutKeepsEvidence(t *testing.T) {
	const marker = `<h1 id="captcha">安全验证</h1>`
	srv := capturePageServer(t, `<html><body>`+marker+`<script>
fetch('/api/health').catch(function(){});
</script></body></html>`)
	pool := newLivePool(t)

	var res *CaptureResult
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		var cerr error
		res, cerr = b.CaptureRequest(context.Background(), srv.URL+"/page", CaptureOptions{
			Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, ".m3u8") },
			CaptureTimeout: 800 * time.Millisecond,
			Page:           PageOptions{Timeout: 5 * time.Second},
		})
		return cerr
	})
	if err == nil {
		t.Fatal("没有匹配请求时应当超时报错")
	}
	if res == nil {
		t.Fatal("失败也要返回现场，不能只给 error")
	}
	if !strings.Contains(res.HTML, "安全验证") {
		t.Fatalf("失败现场应包含当时的页面 HTML，实得 %d 字节", len(res.HTML))
	}
	if res.SeenTotal == 0 || len(res.Seen) == 0 {
		t.Fatalf("应记录未命中的请求样本：total=%d seen=%d", res.SeenTotal, len(res.Seen))
	}
	if res.Timeout != 800*time.Millisecond {
		t.Fatalf("Timeout = %v, want 800ms", res.Timeout)
	}
	if res.Elapsed < 800*time.Millisecond {
		t.Fatalf("Elapsed = %v, 应至少覆盖 CaptureTimeout", res.Elapsed)
	}
}

// 导航本身失败：立刻返回导航错误，不等 CaptureTimeout。
func TestCaptureRequestNavigationError(t *testing.T) {
	pool := newLivePool(t)

	start := time.Now()
	var res *CaptureResult
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		var cerr error
		res, cerr = b.CaptureRequest(context.Background(), "http://127.0.0.1:1/unreachable", CaptureOptions{
			Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, ".m3u8") },
			CaptureTimeout: 5 * time.Second,
			Page:           PageOptions{Timeout: 5 * time.Second},
		})
		return cerr
	})
	if err == nil {
		t.Fatal("不可达地址应当返回导航错误")
	}
	if res == nil {
		t.Fatal("导航失败也要返回结果对象（现场可能为空）")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("导航失败应立刻返回，实耗 %v", elapsed)
	}
}

// ctx 取消：返回 context 错误，页面照样关掉（下一次 Get 能拿到实例）。
func TestCaptureRequestContextCanceled(t *testing.T) {
	srv := capturePageServer(t, `<html><body>no media here</body></html>`)
	pool := newLivePool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := pool.WithBrowser(ctx, func(b *Browser) error {
		_, cerr := b.CaptureRequest(ctx, srv.URL+"/page", CaptureOptions{
			Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, ".m3u8") },
			CaptureTimeout: 10 * time.Second,
		})
		return cerr
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

// 样本有界：页面发出 30+ 条未命中请求，Seen 只留 20 条，总数照实记。
func TestCaptureRequestSeenBounded(t *testing.T) {
	srv := capturePageServer(t, `<html><body><script>
for (var i = 0; i < 30; i++) { fetch('/noise/' + i).catch(function(){}); }
</script></body></html>`)
	pool := newLivePool(t)

	var res *CaptureResult
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		var cerr error
		res, cerr = b.CaptureRequest(context.Background(), srv.URL+"/page", CaptureOptions{
			Match:          func(r CapturedRequest) bool { return strings.HasSuffix(r.URL, ".m3u8") },
			CaptureTimeout: 1200 * time.Millisecond,
			Page:           PageOptions{Timeout: 5 * time.Second},
		})
		return cerr
	})
	if err == nil {
		t.Fatal("没有匹配请求时应当超时")
	}
	if len(res.Seen) != captureSeenLimit {
		t.Fatalf("Seen = %d 条，want %d", len(res.Seen), captureSeenLimit)
	}
	// 30 条脚本请求 + 主文档（Document），至少 31
	if res.SeenTotal < 31 {
		t.Fatalf("SeenTotal = %d, want >= 31", res.SeenTotal)
	}
}

// Match 必填：空谓词属于 API 误用，必须在建页面之前就报错。
func TestCaptureRequestRequiresMatch(t *testing.T) {
	b := &Browser{}
	if _, err := b.CaptureRequest(context.Background(), "https://example.com/", CaptureOptions{}); err == nil {
		t.Fatal("Match 为空应当报错")
	}
}
