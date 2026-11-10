package htmlfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

func TestSiteChallengeHTTPBeforeStatusAndBody(t *testing.T) {
	for _, status := range []int{200, 401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("cf-mitigated", "challenge")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-r.Context().Done() // detecting a challenge must not wait for its body
			}))
			defer srv.Close()
			c := NewClient(Config{Timeout: time.Second, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
			res, err := c.Do(context.Background(), Request{URL: srv.URL + "?token=private"})
			if !errors.Is(err, core.ErrSiteChallenge) || core.Retryable(err) || res == nil || res.StatusCode != status || len(res.Body) != 0 {
				t.Fatalf("response=%+v err=%v", res, err)
			}
			if _, err := c.Fetch(context.Background(), srv.URL); !errors.Is(err, core.ErrSiteChallenge) {
				t.Fatalf("Fetch parsed challenge: %v", err)
			}
		})
	}
}

func TestSiteChallengeCookiesPreserveConcurrentSuccessfulUpdate(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/challenge" {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("cf-mitigated", "challenge")
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "new-valid", Path: "/"})
	}))
	defer srv.Close()
	client := NewClient(Config{Timeout: 3 * time.Second, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	session := NewSession()
	u, _ := url.Parse(srv.URL)
	session.SetCookies(u, []*http.Cookie{{Name: "sid", Value: "old-valid", Path: "/"}})
	ctx, cancel := context.WithTimeout(WithSession(context.Background(), session), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.Do(ctx, Request{URL: srv.URL + "/challenge"}); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := client.Do(ctx, Request{URL: srv.URL + "/refresh"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, core.ErrSiteChallenge) {
		t.Fatal(err)
	}
	cookies := session.Cookies(u)
	if len(cookies) != 1 || cookies[0].Value != "new-valid" {
		t.Fatalf("challenge erased or rolled back successful cookie update: %+v", cookies)
	}
}

func TestSiteChallengeRedirectStopsBeforeCookiesAndFollow(t *testing.T) {
	var followed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			followed.Add(1)
			fmt.Fprint(w, "ordinary final document")
			return
		}
		if r.URL.Path == "/challenge" {
			w.Header().Set("cf-mitigated", "challenge")
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "changed", Path: "/"})
		w.Header().Set("Location", "/final")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	client := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	session := NewSession()
	u, _ := url.Parse(srv.URL)
	session.SetCookies(u, []*http.Cookie{{Name: "sid", Value: "valid", Path: "/"}})
	ctx := WithSession(context.Background(), session)
	res, err := client.Do(ctx, Request{URL: srv.URL + "/challenge?token=private"})
	if !errors.Is(err, core.ErrSiteChallenge) || followed.Load() != 0 || strings.Contains(err.Error(), "private") {
		t.Fatalf("marked redirect followed or leaked request: followed=%d err=%v", followed.Load(), err)
	}
	if res == nil || res.StatusCode != 302 || res.URL.Path != "/challenge" || res.Header.Get("Set-Cookie") == "" {
		t.Fatalf("challenge metadata lost: %+v", res)
	}
	if cookies := session.Cookies(u); len(cookies) != 1 || cookies[0].Value != "valid" {
		t.Fatalf("challenge redirect changed live cookies: %+v", cookies)
	}
	res, err = client.Do(ctx, Request{URL: srv.URL + "/ordinary"})
	if err != nil || followed.Load() != 1 || res.URL.Path != "/final" {
		t.Fatalf("ordinary redirect changed: %+v %v", res, err)
	}
	if cookies := session.Cookies(u); len(cookies) != 1 || cookies[0].Value != "changed" {
		t.Fatalf("ordinary redirect cookie not retained: %+v", cookies)
	}
}

func TestSiteChallengeRequiresExplicitMarker(t *testing.T) {
	for _, marker := range []string{"", "not-challenge", "challenge, arbitrary"} {
		for _, status := range []int{200, 401, 403, 429, 503} {
			if err := DetectSiteChallenge(status, http.Header{"cf-mitigated": {marker}, "Content-Type": {"text/html"}}); err != nil {
				t.Fatalf("ordinary response classified: %d %q", status, marker)
			}
		}
	}
	if err := DetectSiteChallenge(401, http.Header{"CF-MITIGATED": {" challenge "}}); !errors.Is(err, core.ErrSiteChallenge) {
		t.Fatalf("case-insensitive headers lost: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "captcha challenge is ordinary text")
	}))
	defer srv.Close()
	c := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	res, err := c.Do(context.Background(), Request{URL: srv.URL})
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 401 || string(res.Body) != "captcha challenge is ordinary text" {
		t.Fatalf("ordinary authentication path changed: %+v %v", res, err)
	}
}

func TestSiteChallengeDoesNotPoisonProxyHealth(t *testing.T) {
	manager := proxy.NewManager("", 0)
	defer manager.Close()
	health := proxy.DefaultHealthConfig()
	health.Enabled, health.FailureThreshold = true, 1
	if err := manager.ConfigureHealth(health); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(403)
	}))
	defer srv.Close()
	upstream, err := egress.NewProxy(egress.Config{PrivateOrigins: []string{srv.URL, strings.Replace(srv.URL, "http://", "https://", 1)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	client := NewClient(Config{ProxyManager: manager, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	_, err = client.Do(core.WithProxyURL(context.Background(), upstream.URL), Request{URL: srv.URL})
	if !errors.Is(err, core.ErrSiteChallenge) {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("fixed proxy health entry missing: %+v", snapshot)
	}
	for _, status := range snapshot {
		if status.ConsecutiveFailures != 0 || status.InFlight != 0 || status.State != proxy.StateAvailable {
			t.Fatalf("target challenge poisoned proxy: %+v", status)
		}
	}
	if kind := proxy.ClassifyHTTP(upstream.URL, 403, err); kind != proxy.Neutral {
		t.Fatal(kind)
	}
}
