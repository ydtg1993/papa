package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// These fixtures contain only local test data. Production callers must supply
// their own response redactor before any response body is retained.
func ruleSnapshotSourcePolicy() *htmlfetch.SamplePolicy {
	return &htmlfetch.SamplePolicy{RedactBody: func(request bool, body []byte) ([]byte, error) {
		if request {
			return nil, errors.New("snapshot unexpectedly redacted request data")
		}
		return body, nil
	}}
}

func ruleSnapshotSourceObservation(t *testing.T, dir string) ruleSnapshotManifest {
	t.Helper()
	var manifests []ruleSnapshotManifest
	err := filepath.WalkDir(filepath.Join(dir, ruleSnapshotDir), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m ruleSnapshotManifest
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		manifests = append(manifests, m)
		return nil
	})
	if err != nil || len(manifests) != 1 {
		t.Fatalf("expected one observation: count=%d err=%v", len(manifests), err)
	}
	return manifests[0]
}

func ruleSnapshotSourceBody(t *testing.T, dir string, m ruleSnapshotManifest) []byte {
	t.Helper()
	if m.BodyFile == "" || m.BodyOmitted != "" {
		t.Fatalf("complete response was not retained: %+v", m)
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m.BodyFile)))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if m.BodySHA256 != hex.EncodeToString(digest[:]) || m.BodyBytes != len(body) {
		t.Fatalf("stored response digest/size mismatch: %+v", m)
	}
	return body
}

func TestRuleSnapshotSourceHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, omitted string
		status              int
		limit               int64
		retryable           bool
		partial, validation bool
	}{
		{name: "server-error", status: 503, body: `<h1>local temporary failure</h1>`, retryable: true},
		{name: "empty-complete-response", status: 503, body: "", retryable: true},
		{name: "malformed-json", status: 200, body: `local malformed JSON`},
		{name: "invalid-field", status: 200, body: `{"rows":[{"url":"/missing-id"}]}`, validation: true},
		{name: "oversized", status: 200, body: `{"rows":[{"id":"local-long-value"}]}`, limit: 5, omitted: "response-body-too-large"},
		{name: "partial", status: 200, body: `{"rows":[`, partial: true, retryable: true, omitted: "response-body-unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tc.partial {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+100))
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			e, _, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
			e.htmlClient = htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, MaxBodySize: tc.limit})
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			cfg.SnapshotPolicy = ruleSnapshotSourcePolicy()
			cfg.Save = func(context.Context, *Task, RulePage) error { t.Error("failed response reached Save"); return nil }
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/page?token=local-secret"})
			task.ID = 731
			err := e.runAttempt(context.Background(), f, task, 2)
			if err == nil || core.Retryable(err) != tc.retryable || calls.Load() != 1 {
				t.Fatalf("source error changed or response fetched twice: calls=%d err=%v", calls.Load(), err)
			}
			m := ruleSnapshotSourceObservation(t, dir)
			if m.StatusCode != tc.status || m.Rendered || m.SourceURL != srv.URL+"/page" || !m.Failed || m.TaskID != task.ID || m.Attempt != 2 {
				t.Fatalf("incorrect failed HTTP provenance: %+v", m)
			}
			if tc.omitted != "" {
				if m.BodyOmitted != tc.omitted || m.BodyFile != "" || m.OriginalSHA256 != "" {
					t.Fatalf("incomplete response misrepresented as a complete body: %+v", m)
				}
			} else if got := ruleSnapshotSourceBody(t, dir, m); string(got) != tc.body || m.OriginalSHA256 != ruleDigest([]byte(tc.body)) {
				t.Fatalf("failed response body changed: got=%q observation=%+v", got, m)
			}
			if tc.validation && (m.Validation == nil || m.Validation.InvalidRows != 1 || m.Validation.TotalIssues != 1) {
				t.Fatalf("Evaluate failure lost its validation report: %+v", m.Validation)
			}
		})
	}
}

func TestRuleSnapshotSourceAccountFailureAndMaskedPartialBody(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint("partial=", partial), func(t *testing.T) {
			const body = `<h1>local expired account</h1>`
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer local-credential" {
					t.Error("missing pinned authentication or POST method")
				}
				if partial {
					w.Header().Set("Content-Length", "200")
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			e, _, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
			m := account.NewManager(e.htmlClient)
			if err := m.Register(account.Key{Site: "rules", ID: "pinned"}, account.Config{
				Origins: []string{srv.URL},
				Login: func(_ context.Context, c *account.Credentials) error {
					c.Headers["Authorization"] = "Bearer local-credential"
					return nil
				},
				Check:    func(context.Context, *account.Credentials) (bool, error) { return true, nil },
				Classify: func(*htmlfetch.Response, error) account.Outcome { return account.Expired },
			}); err != nil {
				t.Fatal(err)
			}
			e.SetAccountManager(m)
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			cfg.AccountID, cfg.ReplaySafe, cfg.SnapshotPolicy = "pinned", true, ruleSnapshotSourcePolicy()
			cfg.BuildRequest = func(_ context.Context, _ *Task, req RuleRequest) (htmlfetch.Request, error) {
				return htmlfetch.Request{URL: req.URL, Method: http.MethodPost}, nil
			}
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/page"})
			task.ID = 732
			err := e.runAttempt(context.Background(), f, task, 0)
			if !errors.Is(err, account.ErrExpired) || !core.Retryable(err) || calls.Load() != 1 {
				t.Fatalf("account classification or replay changed: calls=%d err=%v", calls.Load(), err)
			}
			observation := ruleSnapshotSourceObservation(t, dir)
			if observation.StatusCode != 401 || !observation.Failed {
				t.Fatalf("account response lost: %+v", observation)
			}
			if partial {
				if observation.BodyFile != "" || observation.OriginalSHA256 != "" || observation.BodyOmitted != "response-body-unavailable" {
					t.Fatalf("account classification made partial body look complete: %+v", observation)
				}
			} else if got := ruleSnapshotSourceBody(t, dir, observation); string(got) != body {
				t.Fatalf("account failure body changed: %q", got)
			}
		})
	}
}

func TestRuleSnapshotSourceNetworkErrorHasNoFabricatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = ruleSnapshotSourcePolicy()
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/unreachable"})
	task.ID = 733
	err := e.runAttempt(context.Background(), f, task, 0)
	if err == nil || !core.Retryable(err) {
		t.Fatalf("network failure classification changed: %v", err)
	}
	m := ruleSnapshotSourceObservation(t, dir)
	if m.BodyFile != "" || m.BodyOmitted != "response-unavailable" || m.OriginalSHA256 != "" || m.StatusCode != 0 || !m.Failed {
		t.Fatalf("network failure fabricated a response body: %+v", m)
	}
}

func TestRuleSnapshotSourceBrowserWaitFailure(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprint("account=", authenticated), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/catalog" {
					calls.Add(1)
				}
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, `<html><body><article><h2>Local wait failure page</h2></article></body></html>`)
			}))
			defer srv.Close()
			pool, err := browser.NewPool(browser.PoolConfig{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			warm, err := pool.Get(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			pool.Put(warm)

			reg := crawlrule.NewRegistry()
			rule := ruleSourceTestRule(t, "html", 0).Rule()
			if err := reg.Register(rule); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(srv.URL + "/fixture")
			if _, err := reg.Activate(rule.ID, rule.Version, []crawlrule.Fixture{{Name: "local", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(`<article><h2>Local fixture</h2></article>`)}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "Local fixture"}}}}}); err != nil {
				t.Fatal(err)
			}
			e, _, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
			e.browserPool = pool
			cfg := RuleFetcherConfig{Site: "rules", Stage: "catalog", Registry: reg, Bindings: map[string]RuleRef{"catalog": {ID: rule.ID, Version: rule.Version}}, Origins: []string{srv.URL}, Transport: "browser", WaitSelector: "#never-appears", SnapshotPolicy: ruleSnapshotSourcePolicy(), Save: func(context.Context, *Task, RulePage) error { t.Error("failed wait reached Save"); return nil }}
			if authenticated {
				manager := account.NewManager(e.htmlClient)
				if err := manager.Register(account.Key{Site: "rules", ID: "pinned"}, account.Config{Origins: []string{srv.URL}, Browser: &account.BrowserConfig{
					Pool: pool, URL: srv.URL + "/bootstrap",
					Login: func(context.Context, *rod.Page) error { return nil },
					Check: func(context.Context, *rod.Page) (bool, error) { return true, nil },
				}}); err != nil {
					t.Fatal(err)
				}
				e.SetAccountManager(manager)
				cfg.AccountID = "pinned"
			}
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/catalog?token=local-secret"})
			task.ID = 734
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = e.runAttempt(ctx, f, task, 0)
			if !errors.Is(err, context.DeadlineExceeded) || !core.Retryable(err) || calls.Load() != 1 {
				t.Fatalf("browser wait classification or request count changed: calls=%d err=%v", calls.Load(), err)
			}
			if !authenticated {
				var waitErr *browser.WaitError
				if !errors.As(err, &waitErr) || waitErr.Stage != "selector" {
					t.Fatalf("anonymous selector wait lost its error type: %v", err)
				}
			}
			m := ruleSnapshotSourceObservation(t, dir)
			if !m.Rendered || m.StatusCode != 0 || m.SourceURL != srv.URL+"/catalog" || !m.Failed {
				t.Fatalf("failed browser provenance incorrect: %+v", m)
			}
			if body := ruleSnapshotSourceBody(t, dir, m); !strings.Contains(string(body), "Local wait failure page") {
				t.Fatalf("readable DOM was lost after canceled wait: %q", body)
			}
		})
	}
}
