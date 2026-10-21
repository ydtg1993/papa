package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// blockFixture 起一个会请求图片 / 两个脚本 / 一个 XHR 的页面，并统计各类请求的命中次数。
type blockFixture struct {
	srv           *httptest.Server
	imageHits     atomic.Int64
	appHits       atomic.Int64
	analyticsHits atomic.Int64
	apiHits       atomic.Int64
}

func newBlockFixture(t *testing.T) *blockFixture {
	t.Helper()
	f := &blockFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body>
<p id="text">blocked-image-page</p>
<img src="/img.png">
<script src="/app.js"></script>
<script src="/analytics.js"></script>
<script>
fetch('/api/data').then(function () {
  document.body.setAttribute('data-done', '1');
});
</script>
</body></html>`))
		case "/img.png":
			f.imageHits.Add(1)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte{0x89, 'P', 'N', 'G'})
		case "/app.js":
			f.appHits.Add(1)
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte("window.__app = true;"))
		case "/analytics.js":
			f.analyticsHits.Add(1)
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte("window.__analytics = true;"))
		case "/api/data":
			f.apiHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// waitDone 等页面里的异步 fetch 完成（body[data-done]）。
func waitDone(t *testing.T, p *rod.Page) {
	t.Helper()
	if _, err := p.Timeout(5 * time.Second).Element("body[data-done]"); err != nil {
		t.Fatalf("等页面异步请求完成：%v", err)
	}
}

// 按资源类型拦图片：图片请求不发出，脚本 / XHR 照常。
func TestBlockResourceTypesPreventsImageRequest(t *testing.T) {
	f := newBlockFixture(t)
	pool := newLivePool(t)

	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), f.srv.URL+"/page", PageOptions{
			Timeout: 10 * time.Second,
			Block: &BlockOptions{
				ResourceTypes: []proto.NetworkResourceType{proto.NetworkResourceTypeImage},
			},
		}, func(p *rod.Page) error {
			waitDone(t, p)
			html, err := p.HTML()
			if err != nil {
				return err
			}
			if !strings.Contains(html, "blocked-image-page") {
				t.Fatal("拦图不该影响文本内容")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithNavigatedPage: %v", err)
	}

	if got := f.imageHits.Load(); got != 0 {
		t.Fatalf("图片请求 = %d 次，want 0（ResourceTypes 拦截没生效）", got)
	}
	if f.apiHits.Load() == 0 {
		t.Fatal("XHR 不该被拦")
	}
	if f.appHits.Load() == 0 {
		t.Fatal("普通脚本不该被拦")
	}
}

// URL 模式拦截统计脚本：命中模式的不发出，其它照常。
func TestBlockURLPatterns(t *testing.T) {
	f := newBlockFixture(t)
	pool := newLivePool(t)

	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), f.srv.URL+"/page", PageOptions{
			Timeout: 10 * time.Second,
			Block:   &BlockOptions{URLPatterns: []string{"*analytics*"}},
		}, func(p *rod.Page) error {
			waitDone(t, p)
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithNavigatedPage: %v", err)
	}

	if got := f.analyticsHits.Load(); got != 0 {
		t.Fatalf("analytics 请求 = %d 次，want 0（URLPatterns 拦截没生效）", got)
	}
	if f.appHits.Load() == 0 || f.apiHits.Load() == 0 {
		t.Fatalf("未命中模式的请求被误伤：app=%d api=%d", f.appHits.Load(), f.apiHits.Load())
	}
}

// nil BlockOptions 是 no-op（现有调用点不受影响）。
func TestBlockNilIsNoop(t *testing.T) {
	if err := applyBlock(nil, nil); err != nil {
		t.Fatalf("nil options 应当安全：%v", err)
	}
}
