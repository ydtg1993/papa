package browser

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

func newWaitServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/selector":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><p>wait-page</p><script>
setTimeout(function () { var d = document.createElement('div'); d.id = 'ready'; document.body.appendChild(d); }, 200);
</script></body></html>`))
		case "/js":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><script>
setTimeout(function () { window.__ready = true; }, 200);
</script></body></html>`))
		case "/idle":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><p>idle-page</p><script>
setTimeout(function () { fetch('/api/ping').catch(function(){}); }, 100);
</script></body></html>`))
		case "/api/ping":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// WaitSelector：等元素出现后才把页面交给回调。
func TestWaitSelector(t *testing.T) {
	srv := newWaitServer(t)
	pool := newLivePool(t)
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), srv.URL+"/selector", PageOptions{
			Timeout:      5 * time.Second,
			WaitSelector: "#ready",
		}, func(p *rod.Page) error {
			if _, err := p.Element("#ready"); err != nil {
				t.Fatalf("WaitSelector 返回时 #ready 应已存在：%v", err)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithNavigatedPage: %v", err)
	}
}

// WaitJS：等 JS 表达式变为 truthy。
func TestWaitJS(t *testing.T) {
	srv := newWaitServer(t)
	pool := newLivePool(t)
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), srv.URL+"/js", PageOptions{
			Timeout: 5 * time.Second,
			WaitJS:  "window.__ready === true",
		}, func(p *rod.Page) error {
			ok, err := p.Eval("() => window.__ready === true")
			if err != nil {
				return err
			}
			if !ok.Value.Bool() {
				t.Fatal("WaitJS 返回时表达式应为 true")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithNavigatedPage: %v", err)
	}
}

// WaitRequestIdle：等页面把延迟请求发完、进入空闲。
func TestWaitRequestIdle(t *testing.T) {
	var pingHits atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/idle" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><script>
setTimeout(function () { fetch('/api/ping'); }, 100);
</script></body></html>`))
			return
		}
		if r.URL.Path == "/api/ping" {
			pingHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer proxy.Close()
	pool := newLivePool(t)

	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), proxy.URL+"/idle", PageOptions{
			Timeout:         10 * time.Second,
			WaitRequestIdle: 300 * time.Millisecond,
		}, func(*rod.Page) error { return nil })
	})
	if err != nil {
		t.Fatalf("WithNavigatedPage: %v", err)
	}
	if pingHits.Load() == 0 {
		t.Fatal("延迟请求应在空闲判定前发出（WaitRequestIdle 等得太早）")
	}
}

// 等待超时：错误必须是 *WaitError，带卡住的步骤、耗时与现场 HTML。
func TestWaitTimeoutKeepsEvidence(t *testing.T) {
	const marker = `<p id="captcha">安全验证</p>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>` + marker + `</body></html>`))
	}))
	defer srv.Close()
	pool := newLivePool(t)

	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), srv.URL, PageOptions{
			Timeout:      500 * time.Millisecond,
			WaitSelector: "#never",
		}, func(*rod.Page) error { return nil })
	})
	if err == nil {
		t.Fatal("等待条件永不满足时应当报错")
	}
	var we *WaitError
	if !errors.As(err, &we) {
		t.Fatalf("错误应是 *WaitError，实得 %T: %v", err, err)
	}
	if we.Stage != "selector" {
		t.Fatalf("Stage = %q, want selector", we.Stage)
	}
	if !strings.Contains(we.HTML, "安全验证") {
		t.Fatalf("WaitError 应带上现场 HTML，实得 %d 字节", len(we.HTML))
	}
	if we.Elapsed <= 0 {
		t.Fatalf("Elapsed = %v, want > 0", we.Elapsed)
	}
}
