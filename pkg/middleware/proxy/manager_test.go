package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/internal/msgqueue"
)

// 没配 api_url 时不建连接、不起刷新协程，Next 返回空串 ——
// "没有代理"必须表现为空串，调用方（rod 的 launcher / htmlfetch 的 proxyFunc）据此跳过代理。
func TestNewManagerWithoutAPIURL(t *testing.T) {
	m := NewManager("", time.Hour)
	if m.client == nil {
		t.Fatal("client 不该为 nil")
	}
	if got := m.Next(); got != "" {
		t.Fatalf("Next = %q, want 空串", got)
	}
}

// NewManager 会同步拉一次代理表，Next 按轮转取值（多 IP 分摊，不总压同一个出口）。
func TestNewManagerFetchesAndRotates(t *testing.T) {
	srv := proxyServer(t, []map[string]string{
		{"host": "1.2.3.4", "port": "8080"},
		{"host": "5.6.7.8", "port": "3128"},
	})

	m := NewManager(srv.URL, time.Hour)

	want := []string{
		"http://1.2.3.4:8080",
		"http://5.6.7.8:3128",
		"http://1.2.3.4:8080", // 转回来
	}
	for i, w := range want {
		if got := m.Next(); got != w {
			t.Fatalf("第 %d 次 Next = %q, want %q", i+1, got, w)
		}
	}

	// 成功也会往错误通道发一条"可用 N 条"的提示 —— 钉住它，免得日后被当成噪声删掉，
	// 那会让"代理表拉到了但内容为空"这种问题失去唯一的现场。
	select {
	case err := <-m.GetErrors():
		if !strings.Contains(err.Error(), "2 available") {
			t.Fatalf("成功路径的提示 = %q", err.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("拉取成功后应在消息通道留一条提示")
	}
}

// 空列表：Next 返回空串而不是 panic（索引取模前先判了长度）。
func TestNextWithEmptyProxyList(t *testing.T) {
	srv := proxyServer(t, nil)
	m := NewManager(srv.URL, time.Hour)
	if got := m.Next(); got != "" {
		t.Fatalf("空代理表时 Next = %q, want 空串", got)
	}
}

// 拉取失败的三条路都要留痕，否则"代理悄悄全部失效"没人知道。
func TestRefreshProxiesErrorPaths(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			"非 200",
			func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
			"status 503",
		},
		{
			"非法 JSON",
			func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) },
			"decode proxy list failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()

			m := NewManager(srv.URL, time.Hour)
			got := drainError(t, m)
			if !strings.Contains(got, c.want) {
				t.Fatalf("错误消息 = %q, want 含 %q", got, c.want)
			}
			if m.Next() != "" {
				t.Fatal("拉取失败时不该有可用代理")
			}
		})
	}
}

// 连不上（服务已关）也要留痕，而不是静默空转。
func TestRefreshProxiesUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关掉，让请求必然失败

	m := NewManager(url, time.Hour)
	if got := drainError(t, m); !strings.Contains(got, "fetch proxies failed") {
		t.Fatalf("错误消息 = %q", got)
	}
}

// 请求都建不出来（URL 畸形）时也要留痕，不能直接 return 掉。
func TestRefreshProxiesBadRequestURL(t *testing.T) {
	m := &Manager{
		apiURL:     "://bad-url",
		client:     &http.Client{Timeout: time.Second},
		trackQueue: msgqueue.NewMsgQueue[any](10),
	}
	m.refreshProxies()

	select {
	case err := <-m.GetErrors():
		if !strings.Contains(err.Error(), "create proxy request failed") {
			t.Fatalf("错误消息 = %q", err.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("建不出请求时应留痕")
	}
}

func proxyServer(t *testing.T, proxies []map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(proxies)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func drainError(t *testing.T, m *Manager) string {
	t.Helper()
	select {
	case err := <-m.GetErrors():
		return err.Error()
	case <-time.After(2 * time.Second):
		t.Fatal("期待一条消息，但通道里没有")
		return ""
	}
}
