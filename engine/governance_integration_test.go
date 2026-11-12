package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

func awaitRuntimeIntegration(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatal("runtime integration condition not reached")
		}
		runtime.Gosched()
	}
}

// Every path below uses the same site but a different stage or account. Fixed
// download/account proxies and twice-rebuilt egress clients exercise the wrapper
// chain that previously could deadlock by acquiring max_inflight=1 twice.
func TestRuntimeSharedRequestBudgetAcrossHTTPAccountFileAndM3U8(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	hits := map[string]int{}
	held := make(chan struct{})
	unblock := make(chan struct{})
	var unblockOnce sync.Once
	defer unblockOnce.Do(func() { close(unblock) })
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		peak = max(peak, active)
		hits[r.URL.Path]++
		mu.Unlock()
		defer func() { mu.Lock(); active--; mu.Unlock() }()
		if r.URL.Path == "/hold" {
			close(held)
			select {
			case <-unblock:
			case <-r.Context().Done():
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/hold", http.StatusFound)
		case "/account":
			if r.Header.Get("Authorization") != "fixture-token" {
				t.Error("account exchange lost its bound identity")
			}
			fmt.Fprint(w, "account")
		case "/playlist.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsegment-a.ts\n#EXTINF:1,\nsegment-b.ts\n#EXT-X-ENDLIST\n")
		case "/file":
			fmt.Fprint(w, "fixture-file")
		default:
			fmt.Fprint(w, "fixture-payload")
		}
	}))
	defer target.Close()
	// The framework deliberately uses CONNECT even for an HTTP destination so
	// the proxy cannot resolve it differently. Its local gateway is the fixture.
	proxyServer, err := egress.NewProxy(egress.Config{PrivateOrigins: []string{target.URL, strings.Replace(target.URL, "http://", "https://", 1)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyServer.Close()
	network := egress.Config{PrivateOrigins: []string{target.URL, proxyServer.URL}}
	mgr := proxy.NewManager("", 0)
	defer mgr.Close()
	e := newTestEngine(t, newFakeTaskDB())
	e.cfg.Network = network
	e.fetchResources.SetHTML(htmlfetch.NewClient(htmlfetch.Config{Network: network, ProxyManager: mgr}))
	cfg := governance.DefaultConfig()
	cfg.Defaults.Request.Enabled = true
	cfg.Defaults.Request.MaxInflight = 1
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(c, nil)
	client := htmlfetch.NewClient(htmlfetch.Config{Network: network, ProxyManager: mgr})
	accounts := account.NewManager(client)
	e.SetAccountManager(accounts)
	key := account.Key{Site: "fixture", ID: "primary"}
	if err := accounts.Register(key, account.Config{Origins: []string{target.URL}, ProxyURL: proxyServer.URL, CheckInterval: time.Hour, Login: func(ctx context.Context, credentials *account.Credentials) error {
		_, err := client.Do(ctx, htmlfetch.Request{URL: target.URL + "/login"})
		if err != nil {
			t.Errorf("fixture login request: %v", err)
		}
		credentials.Headers["Authorization"] = "fixture-token"
		return err
	}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	pools, err := account.NewPools(context.Background(), accounts, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Millisecond, Context: func(ctx context.Context, key account.Key) context.Context { return e.RuntimeContext(ctx, key.Site) }})
	if err != nil {
		t.Fatal(err)
	}
	defer pools.Close()
	poolKey := account.PoolKey{Site: key.Site, Name: "members"}
	if _, err := pools.Upsert(context.Background(), account.PoolConfig{Key: poolKey, Members: []string{key.ID}}, 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := pools.Acquire(ctx, poolKey, account.HTTP); err != nil {
		t.Fatalf("%v; pool=%+v; governor=%+v", err, pools.Snapshot(), c.Snapshot())
	}
	stageContext := func(stage, pool string) context.Context {
		t.Helper()
		sc, release, err := e.prepareRuntimeTask(ctx, &Task{Site: "fixture", Stage: stage, AccountPool: pool}, 4)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		return sc
	}
	plainCtx, accountCtx, fileCtx, mediaCtx := stageContext("catalog", ""), stageContext("detail", poolKey.Name), stageContext("cover", ""), stageContext("media", "")
	fileCfg := filedown.DefaultConfig()
	fileCfg.Network = network
	fileCfg.ProxyManager = mgr
	fileCfg.OutputDir = t.TempDir()
	fileCfg.ResumeStateDir = t.TempDir()
	fileCfg.MaxRetries = 0
	fileCfg.EnableResume = false
	file := filedown.NewDownloader(fileCfg)
	mediaCfg := m3u8.DefaultConfig()
	mediaCfg.Network = network
	mediaCfg.ProxyManager = mgr
	mediaCfg.OutputDir = t.TempDir()
	mediaCfg.ResumeStateDir = t.TempDir()
	mediaCfg.OutputMode = m3u8.OutputSegments
	mediaCfg.AutoMerge = false
	mediaCfg.EnableResume = false
	mediaCfg.MaxConcurrent = 2
	mediaCfg.MaxRetries = 0
	media := m3u8.NewDownloader(mediaCfg)
	// Preserve a context-aware governor underneath a proxy wrapper when client
	// policy is rebuilt by SetClient; this must still be exactly one permit.
	wrapped := egress.Client(&http.Client{Transport: proxy.WrapTransport(governance.WrapTransport(http.DefaultTransport.(*http.Transport).Clone()), mgr)}, network)
	file.SetClient(wrapped)
	media.SetClient(wrapped)
	done := make(chan error, 6)
	go func() { _, err := e.FetchHTTP(plainCtx, htmlfetch.Request{URL: target.URL + "/redirect"}); done <- err }()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		_, err := e.FetchHTTP(accountCtx, htmlfetch.Request{URL: target.URL + "/account"})
		done <- err
	}()
	go func() {
		done <- file.Download(fileCtx, target.URL+"/file", "files", "sample.bin", &filedown.DownloadOptions{Direct: true, Proxy: proxyServer.URL}).Error
	}()
	go func() {
		done <- media.Download(mediaCtx, target.URL+"/playlist.m3u8", "streams", "sample", &m3u8.DownloadOptions{Proxy: proxyServer.URL}).Error
	}()
	go func() {
		done <- file.Download(fileCtx, target.URL+"/file-rebuilt", "files", "rebuilt.bin", &filedown.DownloadOptions{Direct: true}).Error
	}()
	go func() {
		done <- media.Download(mediaCtx, target.URL+"/playlist.m3u8", "streams", "sample-rebuilt").Error
	}()
	awaitRuntimeIntegration(t, func() bool { return c.Snapshot().Sites[0].Waiting == 5 })
	if s := c.Snapshot().Sites[0]; s.Inflight != 1 {
		t.Fatalf("shared admission inflight=%d", s.Inflight)
	}
	unblockOnce.Do(func() { close(unblock) })
	for range 6 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatalf("nested admission deadlock: %v", ctx.Err())
		}
	}
	mu.Lock()
	count := 0
	for _, n := range hits {
		count += n
	}
	observedPeak := peak
	observedHits := fmt.Sprint(hits)
	mu.Unlock()
	if observedPeak != 1 {
		t.Fatalf("paths did not share one request slot: peak=%d hits=%s", observedPeak, observedHits)
	}
	snapshot := c.Snapshot()
	if len(snapshot.Sites) != 1 || snapshot.Sites[0].Site != "fixture" || snapshot.Sites[0].Requests != uint64(count) || snapshot.Sites[0].Inflight != 0 || snapshot.Sites[0].Waiting != 0 {
		t.Fatalf("unscoped, omitted or duplicate exchange accounting: server=%d snapshot=%+v", count, snapshot.Sites)
	}
	data, err := os.ReadFile(filepath.Join(fileCfg.OutputDir, "files", "sample.bin"))
	if err != nil || string(data) != "fixture-file" {
		t.Fatalf("file did not finish: %q %v", data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for path, expected := range map[string]int{"/login": 1, "/redirect": 1, "/hold": 1, "/account": 1, "/file": 1, "/file-rebuilt": 1, "/playlist.m3u8": 2, "/segment-a.ts": 2, "/segment-b.ts": 2} {
		if hits[path] != expected {
			t.Fatalf("expected one exchange for %s, got %v", path, hits)
		}
	}
}

func TestRuntimeCooldownPendingDoesNotReserveGlobalTaskCapacity(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	cfg := governance.DefaultConfig()
	cfg.Defaults.Request.Enabled = true
	cfg.Resources.Enabled = true
	cfg.Resources.TaskMax = 1
	c, err := governance.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(c, nil)
	c.Observe("limited", governance.Feedback{StatusCode: 429, RetryAfter: "60"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, release, err := e.prepareRuntimeTask(ctx, &Task{Site: "limited", Stage: "detail"}, 1)
		if release != nil {
			release()
		}
		done <- err
	}()
	awaitRuntimeIntegration(t, func() bool {
		for _, s := range c.Snapshot().Sites {
			if s.Site == "limited" {
				return s.ReadinessWaiting == 1
			}
		}
		return false
	})
	readyCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_, release, err := e.prepareRuntimeTask(readyCtx, &Task{Site: "ready", Stage: "detail"}, 1)
	if err != nil {
		t.Fatalf("cooled site blocked ready site's task: %v", err)
	}
	release()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := c.Snapshot().Task; got.Inflight != 0 || got.Waiting != 0 {
		t.Fatalf("preclaim wait leaked task capacity: %+v", got)
	}
}

func TestRuntimeStageReleasesAdmissionOnEveryOutcome(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "panic", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			e, _, task, _ := persistenceFixture(t)
			cfg := governance.DefaultConfig()
			cfg.Resources.Enabled = true
			cfg.Resources.TaskMax = 1
			c, err := governance.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			e.InstallGovernance(c, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			info := e.stages[stageKeyOf(task)]
			info.config.WorkerCount = 1
			info.config.MaxAttempts = 1
			info.fetcher = stageRunVersionFetcher(func(context.Context, *Task, *Engine) error {
				switch outcome {
				case "failure":
					return errors.New("fixture failure")
				case "panic":
					panic("fixture panic")
				case "cancel":
					cancel()
					return context.Canceled
				default:
					return nil
				}
			})
			panicked := false
			func() { defer func() { panicked = recover() != nil }(); _ = e.stageHandler(info)(ctx, task) }()
			if panicked != (outcome == "panic") {
				t.Fatalf("panic result=%v", panicked)
			}
			if got := c.Snapshot().Task; got.Inflight != 0 || got.Waiting != 0 {
				t.Fatalf("%s leaked execution permit: %+v", outcome, got)
			}
		})
	}
}
