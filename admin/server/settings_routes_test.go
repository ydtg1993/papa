package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSettingsHandler(t *testing.T) {
	dir := t.TempDir()
	store := &whitelistStoreStub{}
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
		Whitelist:     []string{"10.0.0.0/8"},
		SaveWhitelist: store.save,
		LogDir:        dir,
	})
	body := decodeJSON(t, serveAllowed(m, http.MethodGet, "/api/settings", nil))

	if got := body["whitelist"].([]any); len(got) != 1 || got[0] != "10.0.0.0/8" {
		t.Errorf("whitelist = %v", body["whitelist"])
	}
	// 前端拿 whitelist_source 拼那句"存哪儿"的提示；原来那对 whitelist_file / has_whitelist_file
	// 已经随白名单搬迁去掉了。
	if got := body["whitelist_source"]; got != "crawler_config.ip_whitelist" {
		t.Errorf("whitelist_source = %v, want %s", got, "crawler_config.ip_whitelist")
	}
	if got := body["log_dir"]; got != dir {
		t.Errorf("log_dir = %v, want %s", got, dir)
	}
}

func TestSettingsHandlerWithoutStore(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, Whitelist: []string{"10.0.0.0/8"}})
	rr := serveAllowed(m, http.MethodGet, "/api/settings", nil)
	if got := decodeJSON(t, rr)["whitelist_source"]; got != "" {
		t.Fatalf("没接存储时 whitelist_source 应为空串，实得 %v", got)
	}
}

func TestShutdownHandlerRequiresToken(t *testing.T) {
	newMon := func(fired chan struct{}) *Monitor {
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			OnShutdown: func() { close(fired) },
			VerifyTokenValue: func(token string) (string, bool) {
				if token == "good" {
					return "张三", true
				}
				return "", false
			},
		})
	}
	// 没触发就说明被拦下了 —— 给足一点时间，别把"慢"误判成"被拦"
	assertNotFired := func(t *testing.T, fired chan struct{}, what string) {
		t.Helper()
		select {
		case <-fired:
			t.Fatalf("%s：不该触发关停", what)
		case <-time.After(150 * time.Millisecond):
		}
	}

	t.Run("令牌不对 → 403 且不关停", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{"token":"bad"}`))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rr.Code)
		}
		assertNotFired(t, fired, "令牌不对")
	})

	t.Run("没带令牌 → 403 且不关停", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		if rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{}`)); rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rr.Code)
		}
		assertNotFired(t, fired, "没带令牌")
	})

	t.Run("请求体不是 JSON → 400", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		if rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader("not json")); rr.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rr.Code)
		}
		assertNotFired(t, fired, "请求体非法")
	})

	t.Run("令牌正确 → 关停", func(t *testing.T) {
		fired := make(chan struct{})
		m := newMon(fired)
		rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{"token":"good"}`))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
		}
		select {
		case <-fired:
		case <-time.After(time.Second):
			t.Error("OnShutdown 没在 1s 内触发")
		}
	})

	t.Run("没配校验器 → 不支持关停（fail closed）", func(t *testing.T) {
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, OnShutdown: func() {}})
		rr := serve(m, http.MethodPost, "/api/settings/shutdown", strings.NewReader(`{"token":"good"}`))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
	})
}
