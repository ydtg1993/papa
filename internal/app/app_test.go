package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/internal/auth"
	"strings"
	"testing"
)

// 页面 key / 重复 / 空 Script 都该在注册时报错 —— 启动即失败，别等点了菜单才发现。
func TestUsePageValidation(t *testing.T) {
	bad := []struct {
		name string
		page Page
	}{
		{"key 为空", Page{Script: "Papa.page('x', function(){})"}},
		{"key 含非法字符", Page{Key: "re view", Script: "Papa.page('x', function(){})"}},
		{"Script 为空", Page{Key: "ok"}},
		{"Script 只有空白", Page{Key: "ok", Script: "  \n\t "}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s：应当 panic", c.name)
				}
			}()
			(&App{}).UsePage(c.page)
		})
	}

	t.Run("重复 key", func(t *testing.T) {
		a := &App{}
		a.UsePage(Page{Key: "p", Script: `Papa.page("p", function(){})`})
		defer func() {
			if recover() == nil {
				t.Fatal("重复 key：应当 panic")
			}
		}()
		a.UsePage(Page{Key: "p", Script: `Papa.page("p", function(){})`})
	})
}

// 三条路由：/api/pages 要走宿主注入的鉴权中间件；两个静态端点不走（浏览器标签带不了自定义头）。
func TestMountCustomRoutes(t *testing.T) {
	a := &App{}
	a.UsePage(Page{Key: "review", Label: "审核", Group: "业务",
		Script: `Papa.page("review", function (el) { el.textContent = "hi"; });`})
	a.UseScript(`window.__probe = 1;`)
	a.UsePage(Page{Key: "bare", Script: `Papa.page("bare", function(){});`}) // Label/Group 走默认
	a.UseCSS(`.probe { color: red; }`)

	mux := http.NewServeMux()
	wrapped := 0
	a.mountCustomRoutes(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wrapped++
			next.ServeHTTP(w, r)
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Run("清单", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/pages")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var got struct {
			Pages []map[string]string `json:"pages"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Pages) != 2 {
			t.Fatalf("pages = %v", got.Pages)
		}
		if got.Pages[0]["key"] != "review" || got.Pages[0]["label"] != "审核" || got.Pages[0]["group"] != "业务" {
			t.Errorf("第一页 = %v", got.Pages[0])
		}
		// 留空的 Label/Group 补默认值，前端拿到的永远是具体值
		if got.Pages[1]["key"] != "bare" || got.Pages[1]["label"] != "bare" || got.Pages[1]["group"] != "General" {
			t.Errorf("第二页 = %v", got.Pages[1])
		}
		if wrapped != 1 {
			t.Errorf("/api/pages 应当过鉴权中间件，实际 wrap 调用 %d 次", wrapped)
		}
	})

	t.Run("脚本端点", func(t *testing.T) {
		body := getBody(t, srv.URL+"/static/custom.js")
		for _, want := range []string{`Papa.page("review"`, `Papa.page("bare"`, `window.__probe = 1`} {
			if !strings.Contains(body, want) {
				t.Errorf("custom.js 里缺少 %q：\n%s", want, body)
			}
		}
	})

	t.Run("样式端点", func(t *testing.T) {
		if body := getBody(t, srv.URL+"/static/custom.css"); !strings.Contains(body, ".probe") {
			t.Errorf("custom.css = %q", body)
		}
	})

	t.Run("两个静态端点不该走鉴权", func(t *testing.T) {
		if wrapped != 1 { // 上面已经请求过 /api/pages 一次
			t.Fatalf("静态资源不该触发鉴权中间件，wrap 调用 %d 次", wrapped)
		}
	})

	t.Run("没注入任何东西时返回空体与空清单", func(t *testing.T) {
		mux2 := http.NewServeMux()
		(&App{}).mountCustomRoutes(mux2, nil)
		srv2 := httptest.NewServer(mux2)
		defer srv2.Close()

		if body := getBody(t, srv2.URL+"/static/custom.js"); body != "" {
			t.Errorf("custom.js 应为空，得到 %q", body)
		}
		if body := getBody(t, srv2.URL+"/api/pages"); !strings.Contains(body, `"pages":[]`) {
			t.Errorf("pages 应为空清单，得到 %q", body)
		}
	})

	t.Run("非 GET 的 /api/pages", func(t *testing.T) {
		resp, err := http.Post(srv.URL+"/api/pages", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", resp.StatusCode)
		}
	})
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 审计记人的胶水：操作人来自鉴权中间件写进请求上下文的身份；
// 宿主自己构造的事件没有请求（Req == nil），记空即可，不能让写库出错。
func TestOperatorOf(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/oao/t/action/go", nil)
	req = req.WithContext(auth.WithOperator(req.Context(), "张三"))
	if got := operatorOf(oao.ActionEvent{Req: req}); got != "张三" {
		t.Fatalf("operatorOf = %q, want 张三", got)
	}
	if got := operatorOf(oao.ActionEvent{}); got != "" {
		t.Fatalf("没有请求时应记空，得到 %q", got)
	}
	// 请求在、但中间件没写身份（比如 /monitor 这类只查 IP 的路径）也记空
	plain := httptest.NewRequest(http.MethodPost, "/api/oao/t/action/go", nil)
	if got := operatorOf(oao.ActionEvent{Req: plain}); got != "" {
		t.Fatalf("请求里没有身份时应记空，得到 %q", got)
	}
}
