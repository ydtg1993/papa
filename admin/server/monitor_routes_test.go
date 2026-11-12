package server

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/core"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIMonitor(t *testing.T) {
	m := newFakeMonitor(t)
	rr := serve(m, http.MethodGet, "/api/monitor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)

	stages, ok := body["stages"].(map[string]any)
	if !ok {
		t.Fatalf("stages missing or wrong type: %T", body["stages"])
	}
	for _, name := range []string{"shop:catalog", "shop:detail", "shop:video"} {
		st, ok := stages[name].(map[string]any)
		if !ok {
			t.Fatalf("stage %q missing", name)
		}
		for _, key := range []string{"stage", "site", "global", "workers", "queue"} {
			if _, ok := st[key]; !ok {
				t.Errorf("stage %q missing %q", name, key)
			}
		}
		// 标题要用裸阶段名（站点维度由站点 Tab 表达），而不是 `shop:catalog` 这个键
		if got := st["stage"]; got != "catalog" && got != "detail" && got != "video" {
			t.Errorf("stage %q 的 stage 字段应当是裸阶段名，实得 %v", name, got)
		}
		if got := st["site"]; got != "shop" {
			t.Errorf("stage %q 的 site 字段不对：%v", name, got)
		}
	}

	catalog := stages["shop:catalog"].(map[string]any)
	queue := catalog["queue"].(map[string]any)
	if got := queue["submitted"]; got != float64(8) {
		t.Errorf("catalog submitted = %v, want 8", got)
	}
	if got := queue["completed"]; got != float64(7) {
		t.Errorf("catalog completed = %v, want 7", got)
	}
	if got := queue["failed"]; got != float64(1) {
		t.Errorf("catalog failed = %v, want 1", got)
	}

	global := catalog["global"].(map[string]any)
	if got := global["TotalTasks"]; got != float64(8) {
		t.Errorf("catalog TotalTasks = %v, want 8", got)
	}
	if got := global["TotalFailed"]; got != float64(1) {
		t.Errorf("catalog TotalFailed = %v, want 1", got)
	}

	if workers := catalog["workers"].(map[string]any); len(workers) == 0 {
		t.Error("catalog workers should be non-empty")
	}

	if _, ok := body["custom"]; ok {
		t.Error("custom 不该再出现在 /api/monitor 里 —— 自定义数据模块已整块删除")
	}
	if _, ok := body["system"]; ok {
		t.Error("system should be absent when SysInfo is nil")
	}
}

func TestAPIResponsesNotCached(t *testing.T) {
	ok := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) { return "张三", true },
	})
	deny := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) { return "", false },
	})

	for _, m := range []*Monitor{ok, deny} {
		mux := http.NewServeMux()
		m.Register(mux)
		for _, target := range []string{"/api/monitor", "/api/settings", "/api/logs"} {
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
			if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("%s（%d）的 Cache-Control = %q，want no-store", target, rr.Code, cc)
			}
		}
	}
}

func TestHTMLHandler(t *testing.T) {
	m := newFakeMonitor(t)
	rr := serve(m, http.MethodGet, "/monitor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestMonitorDemoJSON(t *testing.T) {
	m := newFakeMonitor(t)
	rr := serve(m, http.MethodGet, "/api/monitor", nil)

	var v map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	// Keep this synthetic fixture reproducible; live metadata is covered by
	// TestMonitorObservationMetadataIsStablePerInstance instead.
	v["generated_at"] = "2026-11-07T00:00:00Z"
	v["instance_id"] = "synthetic-demo-instance"
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join("testdata", "monitor_demo.json")
	if err := os.WriteFile(out, pretty, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("demo json written to %s", out)
	t.Logf("\n%s", pretty)
}

func TestAPIMonitorCarriesSites(t *testing.T) {
	m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
		SiteStats: func() map[string]core.SiteStat {
			return map[string]core.SiteStat{
				"":      {Key: "", StageCount: 1},
				"siteb": {Key: "siteb", BaseURL: "https://b.example/", StageCount: 2},
			}
		},
	})
	rr := serve(m, http.MethodGet, "/api/monitor", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)
	sites, ok := body["sites"].(map[string]any)
	if !ok || len(sites) != 2 {
		t.Fatalf("sites = %v", body["sites"])
	}
	b, _ := sites["siteb"].(map[string]any)
	if b["base_url"] != "https://b.example/" || b["stage_count"] != float64(2) {
		t.Fatalf("站点快照字段（snake_case）不对：%v", b)
	}

	// 没配 SiteStats 的宿主：这个键应当缺席（前端据此隐藏整排 tab）
	plain := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests})
	if _, ok := decodeJSON(t, serve(plain, http.MethodGet, "/api/monitor", nil))["sites"]; ok {
		t.Fatal("没配 SiteStats 时不该有 sites 键")
	}
}
