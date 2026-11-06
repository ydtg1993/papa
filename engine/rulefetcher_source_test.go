package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/account"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func ruleSourceTestRule(t *testing.T, format string, rows int) *crawlrule.Compiled {
	t.Helper()
	rule := crawlrule.Rule{SchemaVersion: 1, ID: "catalog", Version: "v1", Format: format, MaxRows: rows}
	field := crawlrule.Field{Name: "title", Validation: crawlrule.Validation{Kind: "string", Required: true, MinLength: 1}}
	if format == "html" {
		rule.Rows = "article"
		field.Selector = "h2"
	} else {
		rule.Rows = "/items"
		field.Pointer = "/title"
	}
	rule.Fields = []crawlrule.Field{field}
	c, err := crawlrule.Compile(rule)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ruleSourceTestTask() *Task {
	return &Task{ID: 1, Site: "site", Stage: "catalog", Meta: map[string]string{"rule_id": "catalog", "rule_version": "v1"}}
}

func TestRuleSourceHTTPHTMLAndJSONProvenance(t *testing.T) {
	for format, body := range map[string]string{"html": `<article><h2>Title</h2></article>`, "json": `{"items":[{"title":"Title"}]}`} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Test") != "context" {
					t.Error("context headers missing")
				}
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}}}
			e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{})}
			ctx := core.WithHeaders(context.Background(), map[string]string{"X-Test": "context"})
			got, err := f.readRulePage(ctx, ruleSourceTestTask(), e, RuleRequest{URL: srv.URL + "/"}, ruleSourceTestRule(t, format, 0))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(body))
			if got.StatusCode != 200 || got.Rendered || got.SourceURL != srv.URL+"/" || got.BodySHA256 != hex.EncodeToString(digest[:]) || got.Rule != (RuleRef{ID: "catalog", Version: "v1"}) {
				t.Fatalf("incorrect provenance: %+v", got)
			}
			if len(got.Result.Records) != 1 || got.Result.Records[0]["title"] != "Title" {
				t.Fatalf("incorrect extraction: %+v", got.Result)
			}
		})
	}
}

func TestRuleSourceRedirectNeverForwardsCredentials(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `<article><h2>secret</h2></article>`)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-token" {
			t.Error("initial credential missing")
		}
		http.Redirect(w, r, target.URL+"/private-target", http.StatusFound)
	}))
	defer srv.Close()
	f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL, target.URL}, BuildRequest: func(_ context.Context, _ *Task, r RuleRequest) (htmlfetch.Request, error) {
		return htmlfetch.Request{URL: r.URL, Headers: map[string]string{"Authorization": "Bearer private-token"}}, nil
	}}}
	e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{})}
	_, err := f.readRulePage(context.Background(), ruleSourceTestTask(), e, RuleRequest{URL: srv.URL}, ruleSourceTestRule(t, "html", 0))
	if err == nil || core.Retryable(err) || redirected.Load() != 0 || strings.Contains(err.Error(), "private-") {
		t.Fatalf("redirect followed or unsafe error: followed=%d err=%v", redirected.Load(), err)
	}
}

func TestRuleSourceRequestValidationBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `<article><h2>Title</h2></article>`)
	}))
	defer srv.Close()
	e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{})}
	compiled := ruleSourceTestRule(t, "html", 0)
	for _, tc := range []struct {
		name, method, target string
		replay, want         bool
	}{
		{name: "unsafe-post", method: "POST"},
		{name: "put", method: "PUT", replay: true},
		{name: "changed-path", method: "GET", target: srv.URL + "/other"},
		{name: "changed-origin", method: "GET", target: "http://example.invalid/"},
		{name: "safe-post", method: "POST", replay: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}, ReplaySafe: tc.replay, BuildRequest: func(_ context.Context, _ *Task, r RuleRequest) (htmlfetch.Request, error) {
				if tc.target != "" {
					r.URL = tc.target
				}
				return htmlfetch.Request{URL: r.URL, Method: tc.method, Body: []byte("non-secret-query")}, nil
			}}}
			_, err := f.readRulePage(context.Background(), ruleSourceTestTask(), e, RuleRequest{URL: srv.URL}, compiled)
			if tc.want {
				if err != nil || calls.Load() != before+1 {
					t.Fatalf("safe POST rejected: %v", err)
				}
			} else if err == nil || core.Retryable(err) || calls.Load() != before {
				t.Fatalf("invalid request reached network: calls=%d err=%v", calls.Load()-before, err)
			}
		})
	}
}

func TestRuleSourceHTTPStatusAndBudgets(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		limit     int64
		rows      int
		retryable bool
	}{
		{name: "missing", status: 404, retryable: false},
		{name: "timeout", status: 408, retryable: true},
		{name: "rate-limit", status: 429, retryable: true},
		{name: "server", status: 503, retryable: true},
		{name: "bad-json", status: 200, body: "private-invalid-json", retryable: false},
		{name: "invalid-field", status: 200, body: `{"items":[{"title":17}]}`, retryable: false},
		{name: "too-many-records", status: 200, body: `{"items":[{"title":"A"},{"title":"B"}]}`, rows: 1, retryable: false},
		{name: "oversized-body", status: 200, body: `{"items":[{"title":"private-long-body"}]}`, limit: 10, retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}}}
			e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{MaxBodySize: tc.limit})}
			_, err := f.readRulePage(context.Background(), ruleSourceTestTask(), e, RuleRequest{URL: srv.URL}, ruleSourceTestRule(t, "json", tc.rows))
			if err == nil || core.Retryable(err) != tc.retryable || strings.Contains(err.Error(), "private-") {
				t.Fatalf("classification or redaction failed: %v", err)
			}
		})
	}
}

func TestRuleSourceTransportErrorPreservesIdentityWithoutSecrets(t *testing.T) {
	cause := fmt.Errorf("private-token: %w", context.DeadlineExceeded)
	err := ruleSourceTransportError("HTTP exchange", cause)
	if !errors.Is(err, context.DeadlineExceeded) || !core.Retryable(err) || strings.Contains(err.Error(), "private-") {
		t.Fatalf("error wrapper: %v", err)
	}
	for _, cause := range []error{account.ErrUnknown, account.ErrForbidden, account.ErrChallenge, account.ErrUnsupported} {
		err := ruleSourceTransportError("HTTP exchange", cause)
		if !errors.Is(err, cause) || core.Retryable(err) {
			t.Fatalf("account error misclassified: %v", err)
		}
	}
}

func TestRuleSourceAccountSnapshotAndNoRawArchive(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `<article><h2>private-body</h2></article>`)
	}))
	defer srv.Close()
	e, _, _ := archiveEngine(t, config.ArchiveModeAlways)
	e.htmlClient = htmlfetch.NewClient(htmlfetch.Config{})
	m := account.NewManager(e.htmlClient)
	if err := m.Register(account.Key{Site: "site", ID: "pinned"}, account.Config{Origins: []string{srv.URL}, Login: func(_ context.Context, c *account.Credentials) error {
		c.Headers["Authorization"] = "Bearer private-token"
		return nil
	}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	e.SetAccountManager(m)
	f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}, AccountID: "config-changed"}}
	task := ruleSourceTestTask()
	task.Meta[ruleAccountMeta] = "pinned"
	archive := e.newArchiveBuffer(task, 0)
	trace := newTrace(e.db, logrus.New(), 1, 0)
	ctx := withTrace(withArchive(context.Background(), archive), trace)
	compiled := ruleSourceTestRule(t, "html", 0)
	if _, err := f.readRulePage(ctx, task, e, RuleRequest{URL: srv.URL}, compiled); err != nil {
		t.Fatal(err)
	}
	if auth.Load() != "Bearer private-token" || len(archive.pages) != 0 {
		t.Fatalf("account snapshot/archive: auth=%t archived=%d", auth.Load() != "", len(archive.pages))
	}
	traceJSON, _ := json.Marshal(trace.flush())
	if strings.Contains(string(traceJSON), "private-") {
		t.Fatal("account source leaked raw body or credentials into trace")
	}
	delete(task.Meta, ruleAccountMeta)
	if _, err := f.readRulePage(ctx, task, e, RuleRequest{URL: srv.URL}, compiled); err != nil || auth.Load() != "" {
		t.Fatalf("empty snapshot used config account: auth=%t err=%v", auth.Load() != "", err)
	}
}

func TestRuleSourceBuildRequestCannotChangePinnedAccount(t *testing.T) {
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `<article><h2>Title</h2></article>`)
	}))
	defer srv.Close()
	e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{})}
	m := account.NewManager(e.htmlClient)
	if err := m.Register(account.Key{Site: "site", ID: "pinned"}, account.Config{Origins: []string{srv.URL}, Login: func(_ context.Context, c *account.Credentials) error {
		c.Headers["Authorization"] = "Bearer pinned"
		return nil
	}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	e.SetAccountManager(m)
	for _, changed := range []string{"", "another-account"} {
		t.Run("account="+changed, func(t *testing.T) {
			task := ruleSourceTestTask()
			task.Meta[ruleAccountMeta] = "pinned"
			f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}, BuildRequest: func(_ context.Context, callbackTask *Task, request RuleRequest) (htmlfetch.Request, error) {
				callbackTask.Site = "changed-site"
				callbackTask.Meta[ruleAccountMeta] = changed
				return htmlfetch.Request{URL: request.URL}, nil
			}}}
			_, err := f.readRulePage(context.Background(), task, e, RuleRequest{URL: srv.URL}, ruleSourceTestRule(t, "html", 0))
			if err != nil || auth.Load() != "Bearer pinned" || task.Site != "site" || task.Meta[ruleAccountMeta] != "pinned" {
				t.Fatalf("BuildRequest changed pinned authentication: authenticated=%t err=%v", auth.Load() == "Bearer pinned", err)
			}
		})
	}
}

func TestRuleSourceReplaySafePOSTRefreshesAccountOnNextAttempt(t *testing.T) {
	var requests, logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			t.Error("expected POST")
		}
		if r.Header.Get("Authorization") == "Bearer expired" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed" {
			t.Error("refreshed account missing")
		}
		_, _ = io.WriteString(w, `<article><h2>Title</h2></article>`)
	}))
	defer srv.Close()
	e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{})}
	m := account.NewManager(e.htmlClient)
	err := m.Register(account.Key{Site: "site", ID: "pinned"}, account.Config{Origins: []string{srv.URL}, Login: func(_ context.Context, c *account.Credentials) error {
		c.Headers["Authorization"] = "Bearer refreshed"
		if logins.Add(1) == 1 {
			c.Headers["Authorization"] = "Bearer expired"
		}
		return nil
	}, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	e.SetAccountManager(m)
	f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}, ReplaySafe: true, BuildRequest: func(_ context.Context, _ *Task, request RuleRequest) (htmlfetch.Request, error) {
		return htmlfetch.Request{URL: request.URL, Method: http.MethodPost}, nil
	}}}
	task := ruleSourceTestTask()
	task.Meta[ruleAccountMeta] = "pinned"
	compiled := ruleSourceTestRule(t, "html", 0)
	_, err = f.readRulePage(context.Background(), task, e, RuleRequest{URL: srv.URL}, compiled)
	if !errors.Is(err, account.ErrExpired) || !core.Retryable(err) || requests.Load() != 1 {
		t.Fatalf("expired POST must defer replay to the next attempt: calls=%d err=%v", requests.Load(), err)
	}
	page, err := f.readRulePage(context.Background(), task, e, RuleRequest{URL: srv.URL}, compiled)
	if err != nil || requests.Load() != 2 || logins.Load() != 2 || len(page.Result.Records) != 1 {
		t.Fatalf("next attempt did not refresh: calls=%d logins=%d err=%v", requests.Load(), logins.Load(), err)
	}
}

func TestRuleSourceAccountClassificationOverridesRetryableStatus(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()
			e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{})}
			m := account.NewManager(e.htmlClient)
			err := m.Register(account.Key{Site: "site", ID: "pinned"}, account.Config{
				Origins:  []string{srv.URL},
				Login:    func(context.Context, *account.Credentials) error { return nil },
				Check:    func(context.Context, *account.Credentials) (bool, error) { return true, nil },
				Classify: func(*htmlfetch.Response, error) account.Outcome { return account.Forbidden },
			})
			if err != nil {
				t.Fatal(err)
			}
			e.SetAccountManager(m)
			f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}}}
			task := ruleSourceTestTask()
			task.Meta[ruleAccountMeta] = "pinned"
			_, err = f.readRulePage(context.Background(), task, e, RuleRequest{URL: srv.URL}, ruleSourceTestRule(t, "html", 0))
			if !errors.Is(err, account.ErrForbidden) || core.Retryable(err) {
				t.Fatalf("explicit account outcome replaced by status %d: %v", status, err)
			}
		})
	}
}

func TestRuleSourceBrowserRenderedAndAccount(t *testing.T) {
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true, NoSandbox: true, Leakless: false})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var contextHeader atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" && r.Header.Get("X-Test") == "context" {
			contextHeader.Store(true)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<body><script>setTimeout(() => { document.body.innerHTML='<article><h2>Rendered</h2></article>'; }, 25)</script></body>`)
	}))
	defer srv.Close()
	e, _, _ := archiveEngine(t, config.ArchiveModeAlways)
	e.browserPool = pool
	f := &RuleFetcher{config: RuleFetcherConfig{Origins: []string{srv.URL}, Transport: "browser", WaitSelector: "article h2"}}
	task := ruleSourceTestTask()
	archive := e.newArchiveBuffer(task, 0)
	ctx, cancel := context.WithTimeout(withArchive(core.WithHeaders(context.Background(), map[string]string{"X-Test": "context"}), archive), 45*time.Second)
	defer cancel()
	compiled := ruleSourceTestRule(t, "html", 0)
	page, err := f.readRulePage(ctx, task, e, RuleRequest{URL: srv.URL + "/catalog"}, compiled)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Rendered || page.StatusCode != 0 || page.Result.Records[0]["title"] != "Rendered" || !contextHeader.Load() || len(archive.pages) != 0 {
		t.Fatalf("rendered provenance/header/archive mismatch: %+v", page)
	}
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	err = m.Register(account.Key{Site: "site", ID: "pinned"}, account.Config{Origins: []string{srv.URL}, Browser: &account.BrowserConfig{
		Pool: pool, URL: srv.URL + "/bootstrap",
		Login: func(context.Context, *rod.Page) error { return nil },
		Check: func(context.Context, *rod.Page) (bool, error) { return true, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.SetAccountManager(m)
	task.Meta[ruleAccountMeta] = "pinned"
	page, err = f.readRulePage(ctx, task, e, RuleRequest{URL: srv.URL + "/catalog"}, compiled)
	if err != nil || !page.Rendered || page.StatusCode != 0 || len(archive.pages) != 0 {
		t.Fatalf("account rendered source: %+v %v", page, err)
	}
	var forbiddenCalls atomic.Int32
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbiddenCalls.Add(1) }))
	defer outside.Close()
	f.config.Origins = append(f.config.Origins, outside.URL)
	_, err = f.readRulePage(ctx, task, e, RuleRequest{URL: outside.URL}, compiled)
	if err == nil || core.Retryable(err) || forbiddenCalls.Load() != 0 {
		t.Fatalf("account browser navigated outside its bootstrap origin: calls=%d err=%v", forbiddenCalls.Load(), err)
	}
}
