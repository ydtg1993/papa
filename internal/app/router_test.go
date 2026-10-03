package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// trace 返回一个「路过就记一笔」的中间件，用来断言执行顺序。
func trace(name string, log *[]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*log = append(*log, name)
			next.ServeHTTP(w, r)
		})
	}
}

// denyGuard 冒充后台鉴权：带 X-Deny 头就拒，否则放行。
func denyGuard(calls *int) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*calls++
			if r.Header.Get("X-Deny") != "" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// 中间件正序执行、Group 继承父组中间件与前缀、guard 在最外层（拦下时业务中间件不跑）。
func TestMountRoutersMiddlewareAndGroup(t *testing.T) {
	a := &App{}
	var order []string
	guardCalls, handlerCalls := 0, 0

	a.UseRouter(func(r *Router) {
		r.Use(trace("outer", &order))
		r.Group("/api/demo", func(g *Router) {
			g.Use(trace("inner", &order))
			g.Get("/ping", func(w http.ResponseWriter, _ *http.Request) {
				handlerCalls++
				order = append(order, "handler")
				_, _ = w.Write([]byte("pong"))
			})
			// 子组之后再 Use，只影响子组后续注册的路由
			g.Use(trace("late", &order))
			g.Get("/late", func(w http.ResponseWriter, _ *http.Request) {
				order = append(order, "handler2")
			})
		})
	})

	mux := http.NewServeMux()
	a.mountRouters(mux, denyGuard(&guardCalls))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("命中：业务中间件正序，guard 最先", func(t *testing.T) {
		order, handlerCalls = nil, 0
		if body := getBody(t, srv.URL+"/api/demo/ping"); body != "pong" {
			t.Fatalf("body = %q", body)
		}
		want := []string{"outer", "inner", "handler"}
		if len(order) != len(want) {
			t.Fatalf("执行顺序 = %v, want %v", order, want)
		}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("执行顺序 = %v, want %v", order, want)
			}
		}
		if guardCalls != 1 || handlerCalls != 1 {
			t.Fatalf("guard = %d, handler = %d", guardCalls, handlerCalls)
		}
	})

	t.Run("后注册的中间件只覆盖之后的", func(t *testing.T) {
		order = nil
		getBody(t, srv.URL+"/api/demo/late")
		want := []string{"outer", "inner", "late", "handler2"}
		for i := range want {
			if i >= len(order) || order[i] != want[i] {
				t.Fatalf("执行顺序 = %v, want %v", order, want)
			}
		}
	})

	t.Run("guard 拦下时业务中间件与 handler 都不跑", func(t *testing.T) {
		order, handlerCalls = nil, 0
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/demo/ping", nil)
		req.Header.Set("X-Deny", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if len(order) != 0 {
			t.Errorf("被拒的请求不该进业务中间件，实际走了 %v", order)
		}
		if handlerCalls != 0 {
			t.Errorf("被拒的请求不该进 handler")
		}
	})

	t.Run("路径命中但方法不符 → 405", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/api/demo/ping", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("没注册的路径 → 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/demo/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// NoAuth 出来的组绕过 guard，但前缀照旧生效；其余路由仍受 guard 保护。
func TestRouterNoAuth(t *testing.T) {
	a := &App{}
	guardCalls := 0
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }

	a.UseRouter(func(r *Router) {
		r.Get("/api/pub", ok)
		r.NoAuth().Get("/api/hook", ok)
		r.Group("/api/oauth", func(g *Router) {
			g.NoAuth().Post("/callback", ok) // 前缀跟着副本一起走
		})
	})

	mux := http.NewServeMux()
	a.mountRouters(mux, denyGuard(&guardCalls))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if body := getBody(t, srv.URL+"/api/pub"); body != "ok" {
		t.Fatalf("body = %q", body)
	}
	if guardCalls != 1 {
		t.Fatalf("普通路由应过 guard，实际 %d 次", guardCalls)
	}

	if body := getBody(t, srv.URL+"/api/hook"); body != "ok" {
		t.Fatalf("NoAuth 路由 body = %q", body)
	}
	if guardCalls != 1 {
		t.Fatalf("NoAuth 路由不该过 guard，实际 %d 次", guardCalls)
	}

	resp, err := http.Post(srv.URL+"/api/oauth/callback", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if guardCalls != 1 {
		t.Fatalf("NoAuth 子组不该过 guard，实际 %d 次", guardCalls)
	}
}

// Handle 支持带方法的 pattern，且前缀要插在方法之后（不能拼成 "POST /api/x/y" 之外的东西）。
func TestRouterHandleMethodPrefix(t *testing.T) {
	a := &App{}
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	a.UseRouter(func(r *Router) {
		r.Group("/api/x", func(g *Router) {
			g.Handle("POST /y", http.HandlerFunc(ok))
			g.Handle("/any", http.HandlerFunc(ok)) // 裸路径 = 任意方法
		})
	})

	mux := http.NewServeMux()
	a.mountRouters(mux, nil) // guard 为 nil：不套鉴权也应正常挂载
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/x/y", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/x/y status = %d", resp.StatusCode)
	}

	if resp, err := http.Get(srv.URL + "/api/x/y"); err != nil {
		t.Fatal(err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET /api/x/y status = %d, want 405", resp.StatusCode)
		}
	}

	if body := getBody(t, srv.URL+"/api/x/any"); body != "ok" {
		t.Fatalf("裸路径任意方法应命中，body = %q", body)
	}
}

// 回调为 nil 是误用：启动即失败，别等路由没挂上才发现。
func TestUseRouterNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("UseRouter(nil) 应当 panic")
		}
	}()
	(&App{}).UseRouter(nil)
}

// 一组都没注册时 mountRouters 是 no-op（不能往 mux 上挂东西）。
func TestMountRoutersNoop(t *testing.T) {
	mux := http.NewServeMux()
	(&App{}).mountRouters(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
