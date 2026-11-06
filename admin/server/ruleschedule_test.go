package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/engine"
)

func TestRuleScheduleHTTPGuardsAndErrors(t *testing.T) {
	called := 0
	var failure error
	mon := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{
		VerifyToken: func(r *http.Request) (string, bool) {
			return "operator", r.Header.Get("Authorization") == "Bearer test"
		},
		RuleSchedules: func(ctx context.Context, site string, after uint) (engine.RuleScheduleList, error) {
			called++
			if site != "s" || after != 20 {
				t.Errorf("list arguments: %s %d", site, after)
			}
			return engine.RuleScheduleList{Items: []engine.RuleScheduleTask{}}, failure
		},
		RuleScheduleDetail: func(ctx context.Context, id, after uint, batch string, failed bool) (engine.RuleScheduleDetail, error) {
			called++
			if id != 7 || after != 9 || batch != strings.Repeat("a", 64) || !failed {
				t.Errorf("detail arguments: %d %d %s %v", id, after, batch, failed)
			}
			return engine.RuleScheduleDetail{}, failure
		},
	})
	mux := http.NewServeMux()
	mon.Register(mux)
	request := func(method, url string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, url, nil)
		if auth {
			r.Header.Set("Authorization", "Bearer test")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, url := range []string{"/api/rule-schedules", "/api/rule-schedules/detail?id=7"} {
		if w := request("GET", url, false); w.Code != 401 {
			t.Fatalf("unauthorized: %d", w.Code)
		}
	}
	for _, url := range []string{"/api/rule-schedules?after=-1", "/api/rule-schedules/detail", "/api/rule-schedules/detail?id=0", "/api/rule-schedules/detail?id=1&failed=yes", "/api/rule-schedules?after=18446744073709551616"} {
		if w := request("GET", url, true); w.Code != 400 {
			t.Fatalf("%s: %d", url, w.Code)
		}
	}
	if w := request("POST", "/api/rule-schedules", true); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if called != 0 {
		t.Fatalf("guard invoked backend %d times", called)
	}
	url := "/api/rule-schedules?site=s&after=20"
	if w := request("GET", url, true); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Fatal(w)
	}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{engine.ErrTaskNotFound, 404}, {engine.ErrRuleScheduleInvalid, 400}, {engine.ErrRuleScheduleCorrupt, 409}, {engine.ErrRuleScheduleConflict, 409}, {engine.ErrStageNotRegistered, 409}, {context.DeadlineExceeded, 504}, {errors.New("private SQL details"), 500},
	} {
		failure = tc.err
		w := request("GET", "/api/rule-schedules/detail?id=7&after=9&batch="+strings.Repeat("a", 64)+"&failed=1", true)
		if w.Code != tc.code || strings.Contains(w.Body.String(), "private SQL") {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body.String())
		}
	}
}
