package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type modelSettingsFake struct {
	calls    int
	operator string
	err      error
	dailyErr error
}

func (f *modelSettingsFake) LoadModelDailyUsage(context.Context) (configstore.ModelDailyUsage, error) {
	return configstore.ModelDailyUsage{Day: "2026-11-08", Used: 125, Limit: 1000000, Timezone: "Asia/Shanghai"}, f.dailyErr
}

func (f *modelSettingsFake) LoadModelSettings(context.Context) (configstore.ModelSettings, error) {
	f.calls++
	return configstore.DefaultModelSettings(), f.err
}
func (f *modelSettingsFake) SaveModelSettings(ctx context.Context, m configstore.ModelSettings, operator string) (configstore.ModelSettings, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded save")
	}
	f.calls++
	f.operator = operator
	m.Revision = "1"
	return m, f.err
}
func TestModelSettingsHTTPGuards(t *testing.T) {
	f := &modelSettingsFake{}
	operator := "alice"
	mon := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{ModelSettings: f, VerifyToken: func(r *http.Request) (string, bool) { return operator, r.Header.Get("Authorization") == "Bearer valid" }})
	mux := http.NewServeMux()
	mon.Register(mux)
	raw, _ := json.Marshal(configstore.DefaultModelSettings())
	body := string(raw)
	request := func(method, body string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/settings/model", strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer valid")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, tt := range []struct {
		method, body string
		auth         bool
		code         int
	}{
		{"GET", "", false, 401}, {"POST", body, false, 401}, {"DELETE", "", true, 405},
		{"POST", body + " {}", true, 400}, {"POST", `null`, true, 400}, {"POST", strings.Repeat(" ", 8193), true, 400},
		{"POST", strings.TrimSuffix(body, "}") + `,"enabled":true}`, true, 400},
		{"POST", strings.TrimSuffix(body, "}") + `,"key_ciphertext":"secret"}`, true, 400},
		{"POST", strings.Replace(body, `"enabled":false`, `"enabled":null`, 1), true, 400},
	} {
		w := request(tt.method, tt.body, tt.auth)
		if w.Code != tt.code {
			t.Fatalf("%s: %d %s", tt.method, w.Code, w.Body)
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid request reached storage")
	}
	operator = ""
	if w := request("POST", body, true); w.Code != 403 {
		t.Fatal(w.Code)
	}
	operator = "alice"
	if w := request("POST", body, true); w.Code != 200 || f.operator != "alice" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w := request("POST", strings.TrimSuffix(body, "}")+`,"api_key":"test-private-token"}`, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), "test-private-token") {
		t.Fatalf("write-only key response: %d %s", w.Code, w.Body)
	}
	var view map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &view) != nil || len(view["daily_usage"]) == 0 || len(view["api_key"]) != 0 || len(view["key_ciphertext"]) != 0 {
		t.Fatal("unsafe/incomplete settings response")
	}
	f.dailyErr = errors.New("database secret")
	if w = request("POST", body, true); w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal("failed usage read not safe", w.Code)
	}
	f.dailyErr = nil
	for _, tt := range []struct {
		err  error
		code int
	}{{configstore.ErrModelConflict, 409}, {configstore.ErrModelSettings, 400}, {configstore.ErrModelKeyMissing, 400}, {configstore.ErrModelKeyStorage, 503}, {errors.New("secret DSN"), 503}} {
		f.err = tt.err
		w := request("POST", body, true)
		if w.Code != tt.code || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}
