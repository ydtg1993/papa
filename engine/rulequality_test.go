package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/account"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/egress"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

type qualityRecordFunc func(context.Context, rulequality.Observation) (rulequality.Observation, error)

func (f qualityRecordFunc) Record(ctx context.Context, o rulequality.Observation) (rulequality.Observation, error) {
	return f(ctx, o)
}

func qualityTestCollector(t *testing.T, record qualityRecordFunc) *rulequality.Collector {
	t.Helper()
	c, err := rulequality.NewCollector(record, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func qualityTestCapture(t *testing.T, cfg *RuleFetcherConfig) *[]rulequality.Observation {
	t.Helper()
	var got []rulequality.Observation
	store := rulequality.NewMemoryStore()
	cfg.Quality = qualityTestCollector(t, func(ctx context.Context, o rulequality.Observation) (rulequality.Observation, error) {
		stored, err := store.Record(ctx, o)
		if err != nil {
			t.Errorf("engine emitted invalid observation: %v %+v", err, o)
			return o, err
		}
		got = append(got, o)
		return stored, nil
	})
	return &got
}

func TestRuleQualityExtractionFailureKeepsSummaryButNeverSaves(t *testing.T) {
	const body = `{"rows":[{"url":"/private-value"},{"id":"good"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	got := qualityTestCapture(t, &cfg)
	cfg.Save = func(context.Context, *Task, RulePage) error { t.Error("failed extraction reached Save"); return nil }
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/private-url?key=private-key"})
	if _, err := ruleTestRun(t, f, task); !errors.Is(err, crawlrule.ErrValidation) {
		t.Fatalf("original validation identity lost: %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("observations=%d", len(*got))
	}
	o := (*got)[0]
	if o.FailureKind != rulequality.FailureValidation || o.Operation != "extract" || !o.Evaluated || o.ExtractionOK || o.RawRows != 2 || o.InvalidRows != 1 || o.NewRows != 0 || !o.CompleteResponse || o.HTTPStatus != 200 || o.BodySHA256 != ruleDigest([]byte(body)) {
		t.Fatalf("failed extraction summary: %+v", o)
	}
	var id rulequality.FieldStat
	for _, field := range o.Fields {
		if field.Name == "id" {
			id = field
		}
	}
	if id.Rows != 2 || id.Present != 1 || id.Invalid != 1 || o.FieldsIncomplete {
		t.Fatalf("failed field summary: %+v", o)
	}
	b, _ := json.Marshal(o)
	if strings.Contains(string(b), "private-") || strings.Contains(string(b), srv.URL) {
		t.Fatalf("quality leaked source data: %s", b)
	}
}

func TestRuleQualityFailureNeverReplaysSave(t *testing.T) {
	for _, mode := range []string{"error", "panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"rows":[{"id":"1","url":"/child"}]}`)
			}))
			defer srv.Close()
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			cfg.Quality = qualityTestCollector(t, func(ctx context.Context, o rulequality.Observation) (rulequality.Observation, error) {
				switch mode {
				case "panic":
					panic("private-storage-panic")
				case "timeout":
					<-ctx.Done()
					return o, ctx.Err()
				default:
					return o, errors.New("private-storage-error")
				}
			})
			cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
			saves, submits := 0, 0
			cfg.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
			task.Trace = newTrace(nil, logrus.New(), 1, 0)
			err := f.RunHTTP(context.Background(), task, htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}), func(*Task) error { submits++; return nil })
			if err != nil || saves != 1 || submits != 1 || cfg.Quality.Stats() != (rulequality.CollectorStats{Attempted: 1, Failed: 1}) {
				t.Fatalf("storage side channel changed effects: saves=%d submits=%d err=%v stats=%+v", saves, submits, err, cfg.Quality.Stats())
			}
			trace, _ := json.Marshal(task.Trace.flush())
			if !strings.Contains(string(trace), "record-failed") || strings.Contains(string(trace), "private-") {
				t.Fatalf("warning missing or leaked recorder error: %s", trace)
			}
		})
	}
}

func TestRuleQualitySeparatesPaginationStopAndTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind string
		status           int
		partial          bool
		maxBody          int64
	}{
		{name: "rate", status: 429, body: "private-upstream", kind: rulequality.FailureRateLimit},
		{name: "auth", status: 401, body: "private-upstream", kind: rulequality.FailureAuthentication},
		{name: "forbidden", status: 403, body: "", kind: rulequality.FailureAuthentication},
		{name: "server", status: 503, body: "private-upstream", kind: rulequality.FailureHTTP},
		{name: "network-partial", status: 200, body: "partial", partial: true, kind: rulequality.FailureNetwork},
		{name: "budget", status: 200, body: "oversized", maxBody: 2, kind: rulequality.FailureBudget},
		{name: "invalid-json", status: 200, body: "private-invalid-json", kind: rulequality.FailureExtraction},
		{name: "zero", status: 200, body: `{"rows":[]}`, kind: rulequality.FailureExtraction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.partial {
					w.Header().Set("Content-Length", "500")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			got := qualityTestCapture(t, &cfg)
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
			err := f.RunHTTP(context.Background(), task, htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, MaxBodySize: tc.maxBody}), func(*Task) error { return nil })
			if err == nil || len(*got) != 1 {
				t.Fatalf("err=%v observations=%d", err, len(*got))
			}
			o := (*got)[0]
			complete := !tc.partial && tc.maxBody == 0
			if o.FailureKind != tc.kind || o.HTTPStatus != tc.status || o.CompleteResponse != complete || (o.BodySHA256 != "") != complete {
				t.Fatalf("incorrect transport metrics: %+v", o)
			}
			if tc.name == "zero" && (o.StopReason != "no_records" || o.RawRows != 0) {
				t.Fatalf("zero rows lost: %+v", o)
			}
		})
	}
	for _, reason := range []string{"repeated-content", "no-new-items", "request-cycle"} {
		t.Run(reason, func(t *testing.T) {
			const body = `{"rows":[{"id":"1"}],"next":"/two"}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			got := qualityTestCapture(t, &cfg)
			saves := 0
			cfg.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
			f := ruleTestFetcher(t, cfg)
			req := RuleRequest{URL: srv.URL + "/one"}
			cp := ruleCheckpoint{MaxPages: 100, MaxItems: 100, Requests: []string{ruleDigest([]byte("different request"))}, Bodies: []string{ruleDigest([]byte("different body"))}}
			switch reason {
			case "repeated-content":
				cp.Bodies[0] = ruleDigest([]byte(body))
			case "no-new-items":
				cp.Items = []string{ruleDigest([]byte(ruleJSON("1")))}
			case "request-cycle":
				cp.Requests[0] = ruleDigest([]byte(ruleJSON(req)))
			}
			task, err := f.makeTask("list", req, nil, cfg.Bindings, "", cp)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ruleTestRun(t, f, task); err != nil {
				t.Fatal(err)
			}
			if len(*got) != 1 || saves != 0 {
				t.Fatalf("observations=%d saves=%d", len(*got), saves)
			}
			o := (*got)[0]
			if o.FailureKind != "" || o.StopReason != reason || o.NewRows != 0 || o.Operation != "complete" {
				t.Fatalf("normal stop misclassified: %+v", o)
			}
			if reason != "request-cycle" && (!o.ExtractionOK || o.RawRows != 1) {
				t.Fatalf("raw summary lost: %+v", o)
			}
		})
	}
}

func TestRuleQualityOriginalSummarySurvivesCallbacksAndPartialSubmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"1","url":"/a"},{"id":"2","url":"/b"},{"id":"2","url":"/b"}]}`)
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	got := qualityTestCapture(t, &cfg)
	cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
	cfg.Next = func(_ context.Context, _ *Task, p RulePage) (*RuleRequest, error) {
		delete(p.Result.Records[0], "id")
		p.Result.Validation.InvalidRows = 999
		return nil, nil
	}
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
		for _, row := range p.Result.Records {
			delete(row, "id")
		}
		p.Result.Validation.InvalidRows = 999
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	submitted := 0
	cause := errors.New("queue unavailable")
	err := f.RunHTTP(context.Background(), task, htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}), func(*Task) error {
		submitted++
		if submitted == 2 {
			return cause
		}
		return nil
	})
	if !errors.Is(err, cause) || len(*got) != 1 {
		t.Fatalf("err=%v observations=%d", err, len(*got))
	}
	o := (*got)[0]
	if o.RawRows != 3 || o.NewRows != 2 || o.InvalidRows != 0 || o.FailureKind != rulequality.FailureSubmit || o.PlannedChildren != 2 || o.SubmittedChildren != 1 || !o.ExtractionOK {
		t.Fatalf("callback or partial submission changed metrics: %+v", o)
	}
	for _, field := range o.Fields {
		if field.Name == "id" && (field.Rows != 3 || field.Present != 3) {
			t.Fatalf("raw fields changed: %+v", field)
		}
	}
}

func TestRuleQualitySavePlanningCancelAndPanic(t *testing.T) {
	for _, mode := range []string{"save", "planning", "cancel", "panic", "rule", "host", "budget"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"rows":[{"id":"1","url":"/a"},{"id":"2","url":"/b"}]}`)
			}))
			defer srv.Close()
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			got := qualityTestCapture(t, &cfg)
			cause := errors.New("private-host-error")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			kind := rulequality.FailureSave
			switch mode {
			case "save":
				cfg.Save = func(context.Context, *Task, RulePage) error { return cause }
			case "planning":
				kind = rulequality.FailurePlanning
				cfg.Next = func(context.Context, *Task, RulePage) (*RuleRequest, error) { return nil, cause }
			case "cancel":
				kind = rulequality.FailureCanceled
				cfg.Next = func(context.Context, *Task, RulePage) (*RuleRequest, error) { cancel(); return nil, context.Canceled }
			case "panic":
				kind = rulequality.FailureHost
				cfg.Save = func(context.Context, *Task, RulePage) error { panic(cause) }
			case "rule":
				kind = rulequality.FailureRule
			case "host":
				kind = rulequality.FailureHost
			case "budget":
				kind = rulequality.FailureBudget
				cfg.Links, cfg.MaxChildren = []RuleLink{{Field: "url", Stage: "detail"}}, 1
			}
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
			if mode == "rule" {
				f.config.ItemKey = "not-in-pinned-rule"
			}
			e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})}
			if mode == "host" {
				e.htmlClient = nil
			}
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil && (mode != "panic" || p != cause) {
						t.Fatalf("panic identity changed: %v", p)
					}
				}()
				err = f.Run(ctx, task, e, func(*Task) error { return nil })
				if mode == "panic" {
					t.Error("host panic was swallowed")
				}
			}()
			if mode != "panic" && err == nil {
				t.Error("handler error swallowed")
			}
			if len(*got) != 1 || (*got)[0].FailureKind != kind {
				t.Fatalf("observations: %+v", *got)
			}
			if (mode == "save" || mode == "planning") && !errors.Is(err, cause) {
				t.Fatalf("error identity lost: %v", err)
			}
			if cfg.Quality.Stats().Failed != 0 {
				t.Fatal("canceled handler prevented independent bounded recording")
			}
		})
	}
}

func TestRuleQualityLogicalIdentityAndAttemptWithoutTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"rows":[{"id":"1"}]}`) }))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	got := qualityTestCapture(t, &cfg)
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID, task.Repeat, task.Retry = 42, 3, 4
	e := &Engine{cfg: &config.Config{}, htmlClient: htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})}
	for _, attempt := range []int{0, 2} {
		if err := e.runAttempt(context.Background(), f, task, attempt); err != nil {
			t.Fatal(err)
		}
	}
	first, second := (*got)[0], (*got)[1]
	if first.ID == second.ID || first.LogicalKey == "" || first.LogicalKey != second.LogicalKey || second.Attempt != 2 || second.Reprocess != 4 || second.Repeat != 3 || task.Trace != nil || second.EvidenceID != "" {
		t.Fatalf("attempts without trace/archive: first=%+v second=%+v", first, second)
	}
	task.Repeat++
	if _, err := ruleTestRun(t, f, task); err != nil {
		t.Fatal(err)
	}
	if (*got)[2].LogicalKey == first.LogicalKey {
		t.Fatal("distinct repeat rounds folded")
	}
	task.ID = 0
	for i := 0; i < 2; i++ {
		if _, err := ruleTestRun(t, f, task); err != nil {
			t.Fatal(err)
		}
	}
	if (*got)[3].LogicalKey != "" || (*got)[4].LogicalKey != "" || (*got)[3].ID == (*got)[4].ID {
		t.Fatal("embedded runs collapsed")
	}
	ctx := rulequality.WithLogicalKey(context.Background(), "private-host-round")
	if err := f.RunHTTP(ctx, task, e.htmlClient, func(*Task) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if (*got)[5].LogicalKey != rulequality.LogicalKey(ctx) {
		t.Fatal("explicit host key was lost or rehashed")
	}
}

func TestRuleQualityAccountClassificationUsesIdentity(t *testing.T) {
	for _, outcome := range []account.Outcome{account.Expired, account.Challenge} {
		t.Run(fmt.Sprint(outcome), func(t *testing.T) {
			const body = `{"rows":[{"id":"private-value"}]}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer srv.Close()
			cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
			got := qualityTestCapture(t, &cfg)
			cfg.AccountID, cfg.ReplaySafe = "private-account", true
			cfg.BuildRequest = func(_ context.Context, _ *Task, r RuleRequest) (htmlfetch.Request, error) {
				return htmlfetch.Request{URL: r.URL, Method: "POST"}, nil
			}
			e := &Engine{htmlClient: htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})}
			manager := account.NewManager(e.htmlClient)
			if err := manager.Register(account.Key{Site: "rules", ID: cfg.AccountID}, account.Config{
				Origins: []string{srv.URL}, Login: func(_ context.Context, c *account.Credentials) error {
					c.Headers["Authorization"] = "private-token"
					return nil
				},
				Check:    func(context.Context, *account.Credentials) (bool, error) { return true, nil },
				Classify: func(*htmlfetch.Response, error) account.Outcome { return outcome },
			}); err != nil {
				t.Fatal(err)
			}
			e.SetAccountManager(manager)
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
			err := f.Run(context.Background(), task, e, func(*Task) error { return nil })
			kind := rulequality.FailureAuthentication
			if outcome == account.Challenge {
				kind = rulequality.FailureChallenge
			}
			if err == nil || len(*got) != 1 || (*got)[0].FailureKind != kind || !(*got)[0].CompleteResponse || (*got)[0].HTTPStatus != 200 || (*got)[0].Evaluated {
				t.Fatalf("account classification: %v %+v", err, *got)
			}
			b, _ := json.Marshal((*got)[0])
			if strings.Contains(string(b), "private-") {
				t.Fatalf("account metadata leaked: %s", b)
			}
		})
	}
}

func TestRuleQualityEvidenceAndPinnedRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"1"}]}`)
	}))
	defer srv.Close()
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = ruleSnapshotSourcePolicy()
	got := qualityTestCapture(t, &cfg)
	var saved RulePage
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error { saved = p; return nil }
	f := ruleTestFetcher(t, cfg)
	task, err := f.makeTask("list", RuleRequest{URL: srv.URL}, map[string]string{
		ruleReleaseMeta: ruleJSON(ruleReleaseIdentity{ID: "pinned-release", Generation: 7}),
	}, cfg.Bindings, "", ruleCheckpoint{MaxPages: 100, MaxItems: 10000})
	if err != nil {
		t.Fatal(err)
	}
	task.ID = 451
	// A new fetcher configuration cannot change the task's pinned scope.
	cfg.Bindings = map[string]RuleRef{"list": {ID: "list", Version: "v2"}, "detail": {ID: "detail", Version: "v2"}}
	f = ruleTestFetcher(t, cfg)
	if err := e.runAttempt(context.Background(), f, task, 3); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("observations=%d", len(*got))
	}
	o := (*got)[0]
	manifest := ruleSnapshotSourceObservation(t, dir) // also asserts only one manifest
	if o.EvidenceID == "" || o.EvidenceID != manifest.EvidenceID || o.EvidenceID != saved.EvidenceID || o.Scope.Version != "v1" || o.Scope.ReleaseID != "pinned-release" || o.Scope.ReleaseGeneration != 7 || o.Attempt != 3 {
		t.Fatalf("evidence or pinned release changed: quality=%+v snapshot=%+v saved=%+v", o, manifest, saved)
	}
}

func TestRuleQualityBrowserResponseAndRenderFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("render-failure=", fail), func(t *testing.T) {
			const html = `<html><body><article><h2>Local title</h2></article></body></html>`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if fail {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, html)
			}))
			defer srv.Close()
			pool, err := browser.NewPool(browser.PoolConfig{Network: egress.Config{PrivateOrigins: []string{srv.URL}}, Size: 1, Headless: true, NoSandbox: true, Leakless: false})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()

			reg := crawlrule.NewRegistry()
			rule := ruleSourceTestRule(t, "html", 0).Rule()
			if err := reg.Register(rule); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(srv.URL)
			if _, err := reg.Activate(rule.ID, rule.Version, []crawlrule.Fixture{{Name: "local", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(html)}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "Local title"}}}}}); err != nil {
				t.Fatal(err)
			}
			cfg := RuleFetcherConfig{Site: "rules", Stage: "catalog", Registry: reg, Bindings: map[string]RuleRef{"catalog": {ID: rule.ID, Version: rule.Version}}, Origins: []string{srv.URL}, Transport: "browser", Save: func(context.Context, *Task, RulePage) error { return nil }}
			got := qualityTestCapture(t, &cfg)
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
			e := &Engine{browserPool: pool}
			err = f.Run(context.Background(), task, e, func(*Task) error { return nil })
			if len(*got) != 1 || !(*got)[0].Rendered || (*got)[0].HTTPStatus != 0 {
				t.Fatalf("browser quality: err=%v records=%+v", err, *got)
			}
			o := (*got)[0]
			if fail {
				if err == nil || o.FailureKind != rulequality.FailureRender || o.Evaluated {
					t.Fatalf("render failure classified incorrectly: err=%v observation=%+v", err, o)
				}
			} else if err != nil || !o.CompleteResponse || !o.ExtractionOK || o.RawRows != 1 || o.BodySHA256 == "" {
				t.Fatalf("rendered response lost: err=%v observation=%+v", err, o)
			}
		})
	}
}

func TestRuleQualityNestedDisabledFetcherCannotChangeParent(t *testing.T) {
	const outerBody = `{"rows":[{"id":"outer"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inner" {
			fmt.Fprint(w, `{"rows":[{"id":"inner1"},{"id":"inner2"}]}`)
			return
		}
		fmt.Fprint(w, outerBody)
	}))
	defer srv.Close()
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	inner := ruleTestFetcher(t, cfg)
	innerTask := ruleTestTask(t, inner, RuleRequest{URL: srv.URL + "/inner"})
	client := htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	got := qualityTestCapture(t, &cfg)
	cfg.Save = func(ctx context.Context, _ *Task, _ RulePage) error {
		return inner.RunHTTP(ctx, innerTask, client, func(*Task) error { return nil })
	}
	outer := ruleTestFetcher(t, cfg)
	if _, err := ruleTestRun(t, outer, ruleTestTask(t, outer, RuleRequest{URL: srv.URL + "/outer"})); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].RawRows != 1 || (*got)[0].NewRows != 1 || (*got)[0].BodySHA256 != ruleDigest([]byte(outerBody)) {
		t.Fatalf("disabled nested fetcher changed parent: %+v", *got)
	}
}

func TestRuleQualityNestedEarlyStopDoesNotBorrowParentEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"rows":[{"id":"outer"}]}`)
	}))
	defer srv.Close()
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = ruleSnapshotSourcePolicy()
	got := qualityTestCapture(t, &cfg)
	inner := ruleTestFetcher(t, cfg)
	req := RuleRequest{URL: srv.URL + "/inner"}
	cp := ruleCheckpoint{MaxPages: 100, MaxItems: 100,
		Requests: []string{ruleDigest([]byte(ruleJSON(req)))},
		Bodies:   []string{ruleDigest([]byte("unused"))},
	}
	innerTask, err := inner.makeTask("list", req, nil, cfg.Bindings, "", cp)
	if err != nil {
		t.Fatal(err)
	}
	innerTask.ID = 602
	saves := 0
	cfg.Save = func(ctx context.Context, _ *Task, _ RulePage) error {
		saves++
		return inner.Run(ctx, innerTask, e, func(*Task) error { t.Error("early stop submitted a task"); return nil })
	}
	outer := ruleTestFetcher(t, cfg)
	outerTask := ruleTestTask(t, outer, RuleRequest{URL: srv.URL + "/outer"})
	outerTask.ID = 601
	if err := e.runAttempt(context.Background(), outer, outerTask, 0); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || saves != 1 {
		t.Fatalf("observations=%d saves=%d", len(*got), saves)
	}
	child, parent := (*got)[0], (*got)[1]
	manifest := ruleSnapshotSourceObservation(t, dir)
	if parent.EvidenceID == "" || parent.EvidenceID != manifest.EvidenceID || manifest.TaskID != outerTask.ID {
		t.Fatalf("outer snapshot changed: parent=%+v manifest=%+v", parent, manifest)
	}
	if child.StopReason != "request-cycle" || child.Evaluated {
		t.Fatalf("incorrect child setup: %+v", child)
	}
	if child.EvidenceID != "" {
		t.Fatalf("early-stop child borrowed parent evidence: child_task=%d parent_task=%d child_evidence=%s parent_evidence=%s", child.TaskID, parent.TaskID, child.EvidenceID, parent.EvidenceID)
	}
}

func TestRuleQualityMissingRestoredRegistryVersionStillObserved(t *testing.T) {
	requests, saves, submitted := 0, 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("missing rule made a network request")
	}))
	defer srv.Close()
	full := ruleTestRegistry(t, srv.URL)
	cfg := ruleTestConfig(full, srv.URL)
	got := qualityTestCapture(t, &cfg)
	cfg.Save = func(context.Context, *Task, RulePage) error { saves++; return nil }
	before := ruleTestFetcher(t, cfg)
	restored := crawlrule.NewRegistry()
	fixtureURL, _ := url.Parse(srv.URL + "/fixture")
	for _, id := range []string{"list", "detail"} {
		compiled, err := full.Verified(id, "v2")
		if err != nil {
			t.Fatal(err)
		}
		if err := restored.Register(compiled.Rule()); err != nil {
			t.Fatal(err)
		}
		if _, err := restored.Activate(id, "v2", []crawlrule.Fixture{{Name: "new", Response: &htmlfetch.Response{URL: fixtureURL, StatusCode: 200, Body: []byte(`{"rows":[{"id":"fixture"}]}`)}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/id", Value: "fixture"}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Registry = restored
	cfg.Bindings = map[string]RuleRef{"list": {ID: "list", Version: "v2"}, "detail": {ID: "detail", Version: "v2"}}
	after := ruleTestFetcher(t, cfg)
	for _, cycle := range []bool{false, true} {
		req := RuleRequest{URL: srv.URL + "/old"}
		cp := ruleCheckpoint{MaxPages: 100, MaxItems: 100}
		if cycle {
			cp.Requests = []string{ruleDigest([]byte(ruleJSON(req)))}
			cp.Bodies = []string{ruleDigest([]byte("unused"))}
		}
		oldTask, err := before.makeTask("list", req, nil, before.config.Bindings, "", cp)
		if err != nil {
			t.Fatal(err)
		}
		oldTask.ID = 712
		err = after.RunHTTP(context.Background(), oldTask, htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}), func(*Task) error { submitted++; return nil })
		if !errors.Is(err, crawlrule.ErrVersionNotFound) || core.Retryable(err) {
			t.Fatalf("cycle=%v missing rule error or retry semantics changed: %v", cycle, err)
		}
		if len(*got) == 0 {
			t.Fatalf("missing pinned rule disappeared from quality: observations=%d attempted=%d failed=%d error=%v", len(*got), cfg.Quality.Stats().Attempted, cfg.Quality.Stats().Failed, err)
		}
		o := (*got)[len(*got)-1]
		if o.Scope.Version != "v1" || o.FailureKind != rulequality.FailureRule || o.Operation != "rule" || o.Evaluated || o.EvidenceID != "" {
			t.Fatalf("wrong missing-rule observation: %+v", o)
		}
		// A stale URL or modified pinned reference cannot be used to invent a
		// scope merely because the named version is absent after restart.
		for _, corrupt := range []func(*Task){func(task *Task) { task.URL += "-tampered" }, func(task *Task) { task.Meta["rule_version"] = "bad" }, func(task *Task) { task.Meta[ruleRequestMeta] = "invalid-json" }} {
			bad := cloneRuleTask(oldTask)
			corrupt(bad)
			count, stats := len(*got), cfg.Quality.Stats()
			if err := after.RunHTTP(context.Background(), bad, htmlfetch.NewClient(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}}), func(*Task) error { submitted++; return nil }); err == nil || core.Retryable(err) {
				t.Fatalf("bad identity accepted: %v", err)
			}
			if len(*got) != count || cfg.Quality.Stats() != stats {
				t.Fatal("invalid metadata invented a quality observation")
			}
		}
	}
	if len(*got) != 2 || cfg.Quality.Stats().Attempted != 2 || cfg.Quality.Stats().Failed != 0 || requests != 0 || saves != 0 || submitted != 0 {
		t.Fatalf("observation or business side effects: observations=%d stats=%+v requests=%d saves=%d submitted=%d", len(*got), cfg.Quality.Stats(), requests, saves, submitted)
	}
}
