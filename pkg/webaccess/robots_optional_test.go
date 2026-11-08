package webaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultAccessSkipsRobotsButKeepsLimits(t *testing.T) {
	for _, noRedirect := range []bool{false, true} {
		t.Run(fmt.Sprint(noRedirect), func(t *testing.T) {
			var robots, pages atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					robots.Add(1)
					w.WriteHeader(403)
					return
				}
				pages.Add(1)
				fmt.Fprint(w, "data")
			}))
			defer srv.Close()
			cfg := Config{Policy: Policy{Origins: []Origin{{URL: srv.URL, AllowPrivateNetwork: true}}, MinInterval: 30 * time.Millisecond}, Budgets: Budgets{MaxRequests: 2, MaxPages: 4}}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			get := s.Get
			if noRedirect {
				get = s.GetNoRedirect
			}
			start := time.Now()
			for i := 0; i < 2; i++ {
				r, e := get(context.Background(), srv.URL+"/private")
				if e != nil || string(r.Body) != "data" {
					t.Fatalf("%+v %v", r, e)
				}
			}
			if time.Since(start) < 25*time.Millisecond {
				t.Fatal("configured rate lost")
			}
			if _, err = get(context.Background(), srv.URL+"/private"); !errors.Is(err, ErrBudget) {
				t.Fatalf("request budget lost: %v", err)
			}
			if robots.Load() != 0 || pages.Load() != 2 || s.Usage().Requests != 2 {
				t.Fatalf("robots=%d pages=%d usage=%+v", robots.Load(), pages.Load(), s.Usage())
			}
			cfg.Policy.Origins[0].AllowPrivateNetwork = false
			denied, _ := New(cfg)
			defer denied.Close()
			if _, err = denied.Get(context.Background(), srv.URL); !errors.Is(err, ErrPolicy) {
				t.Fatalf("private permission lost: %v", err)
			}
			cfg.Policy.Origins[0].AllowPrivateNetwork = true
			cfg.Budgets.MaxTotalBytes = 2
			small, _ := New(cfg)
			defer small.Close()
			if _, err = small.Get(context.Background(), srv.URL); !errors.Is(err, ErrBudget) || small.Usage().Bytes != 2 {
				t.Fatalf("byte budget lost: %+v %v", small.Usage(), err)
			}
		})
	}
}

func TestRealChromiumDefaultIgnoresRobotsForPageScriptAndAPI(t *testing.T) {
	var robots atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			robots.Add(1)
			fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
		case "/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, `fetch('/api').then(r=>r.json()).then(x=>document.querySelector('#result').textContent=x.title)`)
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"title":"complete"}`)
		default:
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<div id="result"></div><script src="/app.js"></script>`)
		}
	}))
	defer srv.Close()
	cfg := browserConfig(t, srv.URL)
	cfg.Policy.RespectRobots = false
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	response, records, err := s.Render(context.Background(), srv.URL, RenderOptions{RecordAPI: true, ObserveDuration: 500 * time.Millisecond})
	if err != nil || !strings.Contains(string(response.Body), ">complete</div>") || len(records) != 1 || robots.Load() != 0 {
		t.Fatalf("err=%v robots=%d records=%d body=%s", err, robots.Load(), len(records), response.Body)
	}
}
