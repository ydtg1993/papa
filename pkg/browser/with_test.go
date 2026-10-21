package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// WithPage 交给回调的页面必须在返回后关掉（成功路径）。
func TestWithPageClosesOnReturn(t *testing.T) {
	pool := newLivePool(t)
	var page *rod.Page
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithPage(context.Background(), func(p *rod.Page) error {
			page = p
			if _, err := p.Info(); err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithPage: %v", err)
	}
	if page == nil {
		t.Fatal("回调没拿到页面")
	}
	if _, err := page.Info(); err == nil {
		t.Fatal("WithPage 返回后页面应当已关闭")
	}
}

// 回调 panic 时也要先关页、再归还实例（WithBrowser 的 defer 顺序）。
func TestWithPageClosesOnPanic(t *testing.T) {
	pool := newLivePool(t)
	var page *rod.Page
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic 应当继续向上传播")
			}
		}()
		_ = pool.WithBrowser(context.Background(), func(b *Browser) error {
			return b.WithPage(context.Background(), func(p *rod.Page) error {
				page = p
				panic("boom-in-page-callback")
			})
		})
	}()
	if page == nil {
		t.Fatal("回调没拿到页面")
	}
	if _, err := page.Info(); err == nil {
		t.Fatal("panic 路径也要关页")
	}
}

// WithNavigatedPage：导航成功交给回调，返回后关页；导航失败也不留页面。
func TestWithNavigatedPageClosesOnReturn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>ok</body></html>"))
	}))
	defer srv.Close()

	pool := newLivePool(t)
	var page *rod.Page
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), srv.URL, PageOptions{Timeout: 10 * time.Second}, func(p *rod.Page) error {
			page = p
			html, err := p.HTML()
			if err != nil {
				return err
			}
			if html == "" {
				t.Fatal("导航后应当能读到 HTML")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("WithNavigatedPage: %v", err)
	}
	if page == nil {
		t.Fatal("回调没拿到页面")
	}
	if _, err := page.Info(); err == nil {
		t.Fatal("WithNavigatedPage 返回后页面应当已关闭")
	}
}

// 导航失败时回调不该被调用，实例仍要归还。
func TestWithNavigatedPageReturnsErrorAndKeepsPool(t *testing.T) {
	pool := newLivePool(t)
	called := false
	err := pool.WithBrowser(context.Background(), func(b *Browser) error {
		return b.WithNavigatedPage(context.Background(), "http://127.0.0.1:1/unreachable", PageOptions{Timeout: 2 * time.Second}, func(*rod.Page) error {
			called = true
			return nil
		})
	})
	if err == nil {
		t.Fatal("不可达地址应当返回导航错误")
	}
	if called {
		t.Fatal("导航失败时回调不该被调用")
	}
	// 池位没有被占住：还能再借一个
	if err := pool.WithBrowser(context.Background(), func(*Browser) error { return nil }); err != nil {
		t.Fatalf("导航失败后池子不可用：%v", err)
	}
}
