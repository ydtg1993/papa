package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/governance"
)

func governanceContext(t *testing.T) (context.Context, *governance.Controller) {
	t.Helper()
	cfg := governance.DefaultConfig()
	cfg.Resources.Enabled = true
	cfg.Resources.BrowserMax = 1
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return governance.WithSite(governance.WithController(context.Background(), c), "fixture"), c
}
func awaitGovernance(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("governance condition not reached")
		}
		runtime.Gosched()
	}
}

func TestGovernanceBrowserPhysicalWaitDoesNotHoldGlobalPermit(t *testing.T) {
	ctx, c := governanceContext(t)
	full := newTestPool(1, 1, nil)
	defer full.Close()
	other := newTestPool(1, 1, nil)
	defer other.Close()
	held, err := full.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		b, e := full.Get(waitCtx)
		if b != nil {
			full.Put(b)
		}
		done <- e
	}()
	// Another ready pool can use the sole global permit despite a queued
	// acquisition in a full account-specific pool.
	b, err := other.GetDirect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().Browser.Inflight != 1 {
		t.Fatal("direct browser bypassed resource admission")
	}
	other.Put(b)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	full.Put(held)
	if c.Snapshot().Browser.Inflight != 0 {
		t.Fatal("cancel leaked admission")
	}
}

func TestGovernanceBrowserWaitCancellationFactoryFailureAndPanic(t *testing.T) {
	ctx, c := governanceContext(t)
	held, _ := c.Acquire(ctx, "other", governance.ClassBrowser)
	factory := &countingFactory{}
	p := newTestPool(1, 0, factory.new)
	done := make(chan error, 1)
	go func() { _, err := p.Get(ctx); done <- err }()
	awaitGovernance(t, func() bool { return c.Snapshot().Browser.Waiting == 1 })
	if factory.count() != 0 {
		t.Fatal("browser launched before resource admission")
	}
	p.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed pool admitted pending borrow")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pool close did not cancel resource wait")
	}
	held()
	if c.Snapshot().Browser.Inflight != 0 || p.proxyAlive.Load() != 0 {
		t.Fatal("pool close leaked permit or physical reservation")
	}
	broken := newTestPool(1, 0, func(bool) (*Browser, error) { return nil, fmt.Errorf("factory failed") })
	defer broken.Close()
	if _, err := broken.Get(ctx); err == nil {
		t.Fatal("expected creation error")
	}
	panicking := newTestPool(1, 0, func(bool) (*Browser, error) { panic("factory") })
	defer panicking.Close()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("factory panic swallowed")
			}
		}()
		panicking.Get(ctx)
	}()
	if c.Snapshot().Browser.Inflight != 0 || broken.proxyAlive.Load() != 0 || panicking.proxyAlive.Load() != 0 {
		t.Fatal("creation failure/panic leaked admission")
	}
}

func TestGovernanceBrowserCallbackPanicAndCloseReleaseOnce(t *testing.T) {
	ctx, c := governanceContext(t)
	p := newTestPool(1, 1, nil)
	defer p.Close()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("callback panic swallowed")
			}
		}()
		p.WithDirectBrowser(ctx, func(*Browser) error { panic("callback") })
	}()
	if c.Snapshot().Browser.Inflight != 0 {
		t.Fatal("callback panic leaked admission")
	}
	b, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	b.Close()
	p.Put(b)
	if c.Snapshot().Browser.Inflight != 0 {
		t.Fatal("close/put double released permit")
	}
}

func TestGovernanceRealBrowserNavigationXHRSubresourceRedirectAndBlock(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	var mu sync.Mutex
	starts := []time.Time{}
	hits := map[string]int{}
	var concurrent, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		hits[r.URL.Path]++
		mu.Unlock()
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><img src="/blocked.png"><script src="/script.js"></script><script>Promise.all([fetch('/xhr-a'),fetch('/redirect')]).then(()=>document.body.innerHTML+='<div id="done">done</div>')</script></body></html>`)
		case "/script.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, `window.testReady=true`)
		case "/redirect":
			http.Redirect(w, r, "/xhr-b", 302)
		default:
			time.Sleep(25 * time.Millisecond)
			fmt.Fprint(w, "ok")
		}
	}))
	defer server.Close()
	cfg := governance.DefaultConfig()
	cfg.Defaults.Request.Enabled = true
	cfg.Defaults.Request.MaxInflight = 1
	cfg.Defaults.Request.ManualDelay = 80 * time.Millisecond
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PoolConfig{Network: egress.Config{PrivateOrigins: []string{server.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false, BrowserPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(governance.WithSite(governance.WithController(context.Background(), c), "fixture"), 20*time.Second)
	defer cancel()
	err = pool.WithBrowser(ctx, func(b *Browser) error {
		_, err := b.FetchOnce(ctx, server.URL+"/page", PageOptions{Timeout: 10 * time.Second, WaitSelector: "#done", Block: &BlockOptions{ResourceTypes: []proto.NetworkResourceType{proto.NetworkResourceTypeImage}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["/blocked.png"] != 0 || hits["/script.js"] != 1 || hits["/xhr-a"] != 1 || hits["/redirect"] != 1 || hits["/xhr-b"] != 1 {
		t.Fatalf("unexpected requests: %v", hits)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 55*time.Millisecond {
			t.Fatalf("request bypassed shared interval: %v", gap)
		}
	}
	if peak.Load() > 1 {
		t.Fatalf("request concurrency exceeded: %d", peak.Load())
	}
	awaitGovernance(t, func() bool { return c.Snapshot().Sites[0].Inflight == 0 })
	if c.Snapshot().Sites[0].Requests < uint64(len(starts)) {
		t.Fatal("CDP request accounting omitted exchange")
	}
}

func TestGovernanceRealBrowserCooldownAndCancelledPendingRequests(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	var limitedAt, afterAt time.Time
	var timesMu sync.Mutex
	holdStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cooldown":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><script>fetch('/limited').then(()=>fetch('/after')).then(()=>document.body.innerHTML='<div id="done">done</div>')</script></body></html>`)
		case "/limited":
			timesMu.Lock()
			limitedAt = time.Now()
			timesMu.Unlock()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
		case "/after":
			timesMu.Lock()
			afterAt = time.Now()
			timesMu.Unlock()
			fmt.Fprint(w, "ok")
		case "/cancel":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><script>fetch('/hold');fetch('/queued')</script></body></html>`)
		case "/hold":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			select {
			case holdStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		default:
			fmt.Fprint(w, "ok")
		}
	}))
	defer server.Close()
	cfg := governance.DefaultConfig()
	cfg.Defaults.Request.Enabled = true
	cfg.Defaults.Request.MaxInflight = 1
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PoolConfig{Network: egress.Config{PrivateOrigins: []string{server.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false, BrowserPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(governance.WithSite(governance.WithController(context.Background(), c), "fixture"), 20*time.Second)
	defer cancel()
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		_, err := b.FetchOnce(ctx, server.URL+"/cooldown", PageOptions{WaitSelector: "#done"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	timesMu.Lock()
	gap := afterAt.Sub(limitedAt)
	timesMu.Unlock()
	if gap < 900*time.Millisecond {
		t.Fatalf("browser ignored shared Retry-After: %v", gap)
	}
	if c.Snapshot().Sites[0].RateLimited != 1 {
		t.Fatal("browser omitted 429 observation")
	}
	waitCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- pool.WithBrowser(waitCtx, func(b *Browser) error {
			_, err := b.FetchOnce(waitCtx, server.URL+"/cancel", PageOptions{WaitSelector: "#never"})
			return err
		})
	}()
	select {
	case <-holdStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	awaitGovernance(t, func() bool { return c.Snapshot().Sites[0].Waiting > 0 })
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled page succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled page did not finish")
	}
	awaitGovernance(t, func() bool { s := c.Snapshot().Sites[0]; return s.Inflight == 0 && s.Waiting == 0 })
}

func TestGovernanceRealBrowserThirdPartyExcludedFromTargetLatency(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	var thirdHits atomic.Int32
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thirdHits.Add(1)
		time.Sleep(time.Second)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "third")
	}))
	defer third.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>fetch(%q).then(()=>document.body.innerHTML='<div id="done">done</div>')</script></body></html>`, third.URL)
	}))
	defer main.Close()
	cfg := governance.DefaultConfig()
	cfg.Defaults.Request.Enabled = true
	cfg.Defaults.Request.AutoThrottle.Enabled = true
	cfg.Defaults.Request.AutoThrottle.Smoothing = 1
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PoolConfig{Network: egress.Config{PrivateOrigins: []string{main.URL, third.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false, BrowserPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(governance.WithSite(governance.WithController(context.Background(), c), "fixture"), 20*time.Second)
	defer cancel()
	if err := pool.WithBrowser(ctx, func(b *Browser) error {
		_, err := b.FetchOnce(ctx, main.URL, PageOptions{WaitSelector: "#done"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if thirdHits.Load() != 1 {
		t.Fatalf("third party fixture hits=%d", thirdHits.Load())
	}
	if latency := c.Snapshot().Sites[0].LastLatency; latency >= 500*time.Millisecond {
		t.Fatalf("third-party latency polluted target throttle: %v", latency)
	}
}

func TestGovernanceRealBrowserExistingPageHotEnableDisable(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	var mu sync.Mutex
	starts := map[string][]time.Time{}
	active := map[string]int{}
	peak := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		phase := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		if phase != "off-before" && phase != "on" && phase != "off-after" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>fixture</body></html>")
			return
		}
		mu.Lock()
		starts[phase] = append(starts[phase], time.Now())
		active[phase]++
		peak[phase] = max(peak[phase], active[phase])
		mu.Unlock()
		defer func() { mu.Lock(); active[phase]--; mu.Unlock() }()
		time.Sleep(40 * time.Millisecond)
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	cfg := governance.DefaultConfig()
	cfg.Defaults.Request.Enabled = false
	cfg.Defaults.Request.MaxInflight = 1
	cfg.Defaults.Request.ManualDelay = 120 * time.Millisecond
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PoolConfig{Network: egress.Config{PrivateOrigins: []string{server.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false, BrowserPath: bin})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(governance.WithSite(governance.WithController(context.Background(), c), "fixture"), 15*time.Second)
	defer cancel()
	err = pool.WithBrowser(ctx, func(b *Browser) error {
		page, err := b.NewPage(ctx, server.URL)
		if err != nil {
			return err
		}
		defer page.Context(context.WithoutCancel(ctx)).Timeout(time.Second).Close()
		for _, phase := range []string{"off-before", "on", "off-after"} {
			cfg.Defaults.Request.Enabled = phase == "on"
			if err := c.Update(cfg); err != nil {
				return err
			}
			if _, err := page.Eval(`async phase => { await Promise.all([0,1,2].map(i => fetch('/'+phase+'/'+i).then(r=>r.text()))); return true }`, phase); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, phase := range []string{"off-before", "on", "off-after"} {
		if len(starts[phase]) != 3 {
			t.Fatalf("phase %s requests=%d", phase, len(starts[phase]))
		}
	}
	if peak["off-before"] < 2 || peak["off-after"] < 2 {
		t.Fatalf("disabled control still serialized same-page requests: peaks=%v", peak)
	}
	if peak["on"] != 1 {
		t.Fatalf("hot enable did not cap existing page: peak=%d", peak["on"])
	}
	for i := 1; i < len(starts["on"]); i++ {
		if gap := starts["on"][i].Sub(starts["on"][i-1]); gap < 95*time.Millisecond {
			t.Fatalf("hot enabled interval bypassed: %v", gap)
		}
	}
}
