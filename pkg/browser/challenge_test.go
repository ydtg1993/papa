package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func newChallengePool(t *testing.T, origins ...string) *Pool {
	t.Helper()
	pool, err := NewPool(PoolConfig{Network: egress.Config{PrivateOrigins: origins}, BrowserPath: os.Getenv("PAPA_TEST_CHROME"), Size: 1, Headless: true, NoSandbox: true, Leakless: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func challengeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/challenge-later" {
			<-r.Context().Done() // request-only capture must finish before headers
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if strings.HasPrefix(r.URL.Path, "/challenge") {
			w.Header().Set("cf-mitigated", "challenge")
		}
		status, _ := strconv.Atoi(r.URL.Query().Get("status"))
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		switch r.URL.Path {
		case "/challenge-hang":
			// Cross the framework proxy's HTTP write buffer so Chrome really
			// receives the headers before the response body stalls.
			fmt.Fprint(w, `<html><body>challenge`+strings.Repeat(" ", 8192))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/selected":
			fmt.Fprintf(w, `<script>fetch('/challenge-api?status=%d')</script>`, status)
		case "/noise":
			fmt.Fprint(w, `<iframe src="/challenge-frame?status=403"></iframe><img src="/challenge-image?status=403"><div id="ready">ready</div>`)
		default:
			fmt.Fprint(w, `<div id="ready">captcha is only a word</div>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func assertSiteChallenge(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, core.ErrSiteChallenge) || core.Retryable(err) || core.ErrorKind(err) != "site_challenge" {
		t.Fatalf("expected nonretryable site challenge, got %v", err)
	}
}

func TestSiteChallengeManagedNavigationStatusAndWaits(t *testing.T) {
	srv := challengeServer(t)
	pool := newChallengePool(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := pool.WithBrowser(ctx, func(b *Browser) error {
		for _, status := range []int{200, 401, 403} {
			_, err := b.FetchOnce(ctx, fmt.Sprintf("%s/challenge?status=%d", srv.URL, status), PageOptions{Timeout: 2 * time.Second})
			assertSiteChallenge(t, err)
			page, err := b.NewPageWithOptions(ctx, fmt.Sprintf("%s/normal?status=%d", srv.URL, status), PageOptions{Timeout: 2 * time.Second})
			if err != nil {
				return fmt.Errorf("ordinary %d: %w", status, err)
			}
			_ = page.Close()
		}
		_, err := b.FetchOnce(ctx, srv.URL+"/challenge-hang", PageOptions{Timeout: 150 * time.Millisecond})
		assertSiteChallenge(t, err)
		_, err = b.NewPageWithOptions(ctx, srv.URL+"/challenge", PageOptions{WaitSelector: "#never", Timeout: 150 * time.Millisecond})
		assertSiteChallenge(t, err)
		_, err = b.FetchOnce(ctx, srv.URL+"/noise", PageOptions{Timeout: 2 * time.Second, WaitSelector: "#ready"})
		if err != nil {
			return fmt.Errorf("unrelated frame/resource challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSiteChallengeSelectedResponseAndCaptureRequestBoundaries(t *testing.T) {
	srv := challengeServer(t)
	pool := newChallengePool(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := pool.WithBrowser(ctx, func(b *Browser) error {
		for _, status := range []int{200, 401, 403} {
			response, err := b.CaptureResponse(ctx, fmt.Sprintf("%s/selected?status=%d", srv.URL, status), ResponseOptions{Timeout: 3 * time.Second, Match: func(r CapturedResponse) bool { return strings.Contains(r.Request.URL, "/challenge-api") }})
			assertSiteChallenge(t, err)
			if response == nil || response.StatusCode != status {
				t.Fatalf("selected response metadata missing: %+v", response)
			}
		}
		_, err := b.CaptureResponse(ctx, srv.URL+"/challenge-hang", ResponseOptions{Timeout: 2 * time.Second, Match: func(r CapturedResponse) bool { return r.Request.ResourceType == proto.NetworkResourceTypeDocument }})
		assertSiteChallenge(t, err)
		match := func(r CapturedRequest) bool { return r.ResourceType == proto.NetworkResourceTypeDocument }
		start := time.Now()
		request, err := b.CaptureRequest(ctx, srv.URL+"/challenge-later", CaptureOptions{Match: match, CaptureTimeout: time.Second})
		if err != nil || request.Request == nil || request.ResponseObserved || time.Since(start) >= time.Second {
			t.Fatalf("request-only capture lost early success: %+v %v", request, err)
		}
		request, err = b.CaptureRequest(ctx, srv.URL+"/challenge", CaptureOptions{Match: match, WaitLoad: true, Page: PageOptions{Timeout: 2 * time.Second}})
		assertSiteChallenge(t, err)
		if request.Request == nil || !request.ResponseObserved {
			t.Fatalf("WaitLoad evidence missing: %+v", request)
		}
		_, err = b.CaptureRequest(ctx, srv.URL+"/challenge", CaptureOptions{Match: func(CapturedRequest) bool { return false }, CaptureTimeout: 150 * time.Millisecond})
		assertSiteChallenge(t, err)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSiteChallengeThirdPartyResourcesIgnored(t *testing.T) {
	thirdParty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		fmt.Fprint(w, "challenge")
	}))
	defer thirdParty.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<iframe src="%s/frame"></iframe><img src="%s/image"><p>main response is ordinary</p>`, thirdParty.URL, thirdParty.URL)
	}))
	defer main.Close()
	pool := newChallengePool(t, main.URL, thirdParty.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		_, err := b.FetchOnce(ctx, main.URL, PageOptions{Timeout: 3 * time.Second})
		return err
	}); err != nil {
		t.Fatalf("third-party challenge rejected ordinary main document: %v", err)
	}
}

func TestSiteChallengeIdentityStateGovernanceCoexist(t *testing.T) {
	srv := challengeServer(t)
	pool := newChallengePool(t, srv.URL)
	ctx, controller := governanceContext(t)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		profile := identity.Profile{Site: "fixture", ID: "challenge", Version: 1, SchemaVersion: identity.SchemaVersion, UAPolicy: identity.NativeChromium, Languages: []string{"en-US"}, Locale: "en-US", Timezone: "UTC", Viewport: identity.Size{Width: 800, Height: 600}, Screen: identity.Size{Width: 1280, Height: 720}, DeviceScaleFactor: 1, Adapter: identity.AdapterConfig{ID: "webdriver", Version: "1", Enabled: true}}
		resolved, err := b.ResolveIdentity(ctx, profile)
		if err != nil {
			return err
		}
		return b.WithStatePage(ctx, srv.URL+"/normal", navigationState(srv.URL, "saved"), PageOptions{Identity: resolved, Timeout: 5 * time.Second, Block: &BlockOptions{ResourceTypes: []proto.NetworkResourceType{proto.NetworkResourceTypeImage}}}, func(page *rod.Page) error {
			value, err := page.Eval(`() => localStorage.getItem('token') === 'saved' && navigator.webdriver === false`)
			if err != nil {
				return err
			}
			if !value.Value.Bool() {
				t.Fatal("state restore and identity adapter did not coexist")
			}
			_, err = CaptureResponsePage(ctx, page, srv.URL+"/selected", ResponseOptions{Page: PageOptions{Identity: resolved}, Timeout: 3 * time.Second, Match: func(r CapturedResponse) bool { return strings.Contains(r.Request.URL, "/challenge-api") }})
			assertSiteChallenge(t, err)
			result, err := CaptureRequestPage(ctx, page, srv.URL+"/challenge", CaptureOptions{Page: PageOptions{Identity: resolved}, Match: func(r CapturedRequest) bool { return r.ResourceType == proto.NetworkResourceTypeDocument }})
			assertSiteChallenge(t, err)
			if result == nil || !result.ResponseObserved {
				t.Fatal("managed capture lost observed response")
			}
			return err
		})
	})
	assertSiteChallenge(t, err)
	if controller.Snapshot().Browser.Inflight != 0 {
		t.Fatal("challenge leaked browser admission")
	}
}

func TestSiteChallengeStateAndRecordDoNotCallBusinessSuccess(t *testing.T) {
	srv := challengeServer(t)
	pool := newChallengePool(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	called := false
	err := pool.WithIsolatedBrowser(ctx, func(b *Browser) error {
		return b.WithStatePage(ctx, srv.URL+"/challenge", navigationState(srv.URL, "saved"), PageOptions{Timeout: 3 * time.Second}, func(*rod.Page) error { called = true; return nil })
	})
	assertSiteChallenge(t, err)
	if called {
		t.Fatal("business callback received challenge page")
	}
	err = pool.WithBrowser(ctx, func(b *Browser) error {
		_, err := b.RecordNetwork(ctx, srv.URL+"/challenge", RecordOptions{Timeout: 3 * time.Second, ObserveFor: time.Millisecond, Page: PageOptions{Interact: func(context.Context, *rod.Page) error { called = true; return nil }}})
		return err
	})
	assertSiteChallenge(t, err)
	if called {
		t.Fatal("record interacted with challenge page")
	}
}
