package webaccess

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-rod/rod/lib/cdp"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrowserPlanBoundsAndProgress(t *testing.T) {
	for _, p := range []BrowserPlan{
		{Wait: &BrowserWait{Selector: "div", State: "visible", TimeoutMS: 30001}},
		{Scroll: &BrowserScroll{ItemSelector: "a", KeyAttribute: "href", MaxRounds: 13, IntervalMS: 500, StableRounds: 1}},
		{Pagination: &BrowserPagination{Selector: "a", Mode: "script", MaxPages: 2, WaitMS: 500}},
		{Pagination: &BrowserPagination{Selector: "a", Mode: "link", MaxPages: 9, WaitMS: 500}},
	} {
		if !errors.Is(p.Validate(), ErrBrowserPlan) {
			t.Fatal("invalid plan accepted")
		}
	}
	if (BrowserPlan{}).Validate() != nil {
		t.Fatal("explicit noninteractive plan rejected")
	}
	if newBrowserKeys(`["a","b"]`, `["b","a"]`) || !newBrowserKeys(`["a"]`, `["a","b"]`) {
		t.Fatal("progress is not based on new keys")
	}
}

func TestRealChromiumBrowseEvidenceAndBounds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/one":
			fmt.Fprint(w, `<a class="item" href="/detail">first</a><a id="next" href="/two">next</a>`)
		case "/two":
			fmt.Fprint(w, `<a class="item" href="/detail2">second</a>`)
		case "/button":
			fmt.Fprint(w, `<div id="value">first</div><button id="next" onclick="document.getElementById('value').textContent='second';history.pushState({},'', '/done');this.remove()">next</button>`)
		case "/scroll":
			fmt.Fprint(w, `<div style="height:2000px"><a class="item" href="/d1">first</a></div><script>addEventListener('scroll',()=>{if(!document.getElementById('added')){let a=document.createElement('a');a.id='added';a.className='item';a.href='/d2';a.textContent='second';document.body.append(a)}})</script>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name, path string
		plan       BrowserPlan
		pages      int
		stop       string
		truncated  bool
	}{
		{"link", "/one", BrowserPlan{Pagination: &BrowserPagination{Selector: "#next", Mode: "link", MaxPages: 2, WaitMS: 500}}, 2, "no_next", false},
		{"cap", "/one", BrowserPlan{Pagination: &BrowserPagination{Selector: "#next", Mode: "link", MaxPages: 1, WaitMS: 500}}, 1, "page_limit", true},
		{"button", "/button", BrowserPlan{Pagination: &BrowserPagination{Selector: "#next", Mode: "button", MaxPages: 2, WaitMS: 500}}, 2, "no_next", false},
		{"scroll", "/scroll", BrowserPlan{Wait: &BrowserWait{Selector: ".item", State: "visible", TimeoutMS: 2000}, Scroll: &BrowserScroll{ItemSelector: ".item", KeyAttribute: "href", MaxRounds: 4, IntervalMS: 500, StableRounds: 2}}, 1, "complete", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := browserConfig(t, srv.URL)
			cfg.Policy.RespectRobots = false
			cfg.Policy.ResourceOrigins = cfg.Policy.Origins
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			out, err := s.Browse(context.Background(), srv.URL+tc.path, tc.plan)
			if err != nil || len(out.Pages) != tc.pages || out.StopReason != tc.stop || out.Truncated != tc.truncated {
				t.Fatalf("pages=%d stop=%s truncated=%v error=%v", len(out.Pages), out.StopReason, out.Truncated, err)
			}
			for _, p := range out.Pages {
				if !p.Rendered || p.StatusCode != 0 || len(p.Body) == 0 {
					t.Fatal("invalid DOM evidence")
				}
			}
			if tc.name == "button" && out.Pages[1].URL != srv.URL+"/done" {
				t.Fatal("same-document URL lost")
			}
			if tc.name == "scroll" && !strings.Contains(string(out.Pages[0].Body), `id="added"`) {
				t.Fatal("did not scroll")
			}
			if s.Usage().Pages != tc.pages {
				t.Fatalf("incorrect page accounting %+v", s.Usage())
			}
		})
	}
}

func TestRealChromiumBrowseHiddenResourceGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hidden" {
			hits.Add(1)
			fmt.Fprint(w, "hidden")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<img src="/hidden"><p>public</p>`)
	}))
	defer srv.Close()
	cfg := browserConfig(t, srv.URL)
	cfg.Policy.RespectRobots = false
	cfg.Policy.ResourceOrigins = cfg.Policy.Origins
	cfg.CheckURL = func(raw string) error {
		if strings.HasSuffix(raw, "/hidden") {
			return ErrPolicy
		}
		return nil
	}
	s, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	_, e = s.Browse(context.Background(), srv.URL, BrowserPlan{})
	if !errors.Is(e, ErrPolicy) || hits.Load() != 0 {
		t.Fatalf("guard failed: %v hits=%d", e, hits.Load())
	}
}

func TestCanceledInterceptionClassification(t *testing.T) {
	if !canceledInterception(&cdp.Error{Code: -32602, Message: "Invalid InterceptionId."}) {
		t.Fatal("cancelled resource not recognized")
	}
	for _, e := range []error{ErrPolicy, ErrBudget, &cdp.Error{Code: -32602, Message: "Other protocol failure"}, &cdp.Error{Code: -32000, Message: "Invalid InterceptionId."}} {
		if canceledInterception(e) {
			t.Fatal("other error suppressed")
		}
	}
}
func TestRealChromiumBrowseSharedBudgetCycleAndCancel(t *testing.T) {
	var second atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/two" {
			second.Add(1)
			fmt.Fprint(w, `<p>two</p><a id="next" href="/one">next</a>`)
			return
		}
		fmt.Fprint(w, `<p>one</p><a id="next" href="/two">next</a>`)
	}))
	defer srv.Close()
	plan := BrowserPlan{Pagination: &BrowserPagination{Selector: "#next", Mode: "link", MaxPages: 4, WaitMS: 500}}
	for _, kind := range []string{"budget", "cycle", "cancel", "hidden_redirect"} {
		t.Run(kind, func(t *testing.T) {
			cfg := browserConfig(t, srv.URL)
			cfg.Policy.RespectRobots = false
			cfg.Policy.ResourceOrigins = cfg.Policy.Origins
			if kind == "budget" {
				cfg.Budgets.MaxPages = 1
			}
			if kind == "hidden_redirect" {
				cfg.CheckURL = func(u string) error {
					if strings.HasSuffix(u, "/two") {
						return ErrPolicy
					}
					return nil
				}
			}
			s, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			ctx := context.Background()
			if kind == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			before := second.Load()
			out, e := s.Browse(ctx, srv.URL+"/one", plan)
			switch kind {
			case "budget":
				if !errors.Is(e, ErrBudget) || second.Load() != before || s.Usage().Pages != 1 {
					t.Fatalf("budget bypass: %v %+v", e, s.Usage())
				}
			case "cycle":
				if e != nil || !out.Truncated || out.StopReason != "repeated_url" || len(out.Pages) != 2 {
					t.Fatalf("cycle not bounded %+v %v", out, e)
				}
			case "cancel":
				if !errors.Is(e, ErrCanceled) {
					t.Fatalf("cancellation lost: %v", e)
				}
			case "hidden_redirect":
				if !errors.Is(e, ErrPolicy) || second.Load() != before {
					t.Fatalf("protected next page reached %v", e)
				}
			}
		})
	}
}
func TestURLGuardRejectsHTTPRedirectBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hidden" {
			hits.Add(1)
			return
		}
		http.Redirect(w, r, "/hidden", 302)
	}))
	defer srv.Close()
	cfg := localConfig(srv.URL)
	cfg.Policy.RespectRobots = false
	cfg.CheckURL = func(u string) error {
		if strings.HasSuffix(u, "/hidden") {
			return ErrPolicy
		}
		return nil
	}
	s, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.Get(context.Background(), srv.URL); !errors.Is(e, ErrPolicy) || hits.Load() != 0 {
		t.Fatalf("hidden redirect reached: %v", e)
	}
}

func TestRealChromiumBrowseFragmentLinkCannotBypassPageBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<p id="item">first</p><a id="next" href="#second">next</a><script>onhashchange=()=>{document.getElementById('item').textContent='second';document.getElementById('next').remove()}</script>`)
	}))
	defer srv.Close()
	cfg := browserConfig(t, srv.URL)
	cfg.Policy.RespectRobots = false
	cfg.Policy.ResourceOrigins = cfg.Policy.Origins
	cfg.Budgets.MaxPages = 1
	s, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	out, e := s.Browse(context.Background(), srv.URL+"/one", BrowserPlan{Pagination: &BrowserPagination{Selector: "#next", Mode: "link", MaxPages: 2, WaitMS: 500}})
	if e != nil || out.StopReason != "repeated_url" || !out.Truncated || len(out.Pages) != 1 {
		t.Fatalf("fragment link bypassed navigation identity: pages=%d usage=%+v stop=%s err=%v", len(out.Pages), s.Usage(), out.StopReason, e)
	}
}
