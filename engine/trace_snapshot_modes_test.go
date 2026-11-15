package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/tracestore"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func snapshotTokens(t *testing.T, e *Engine, id int) []string {
	t.Helper()
	steps, err := e.ListTraceExecution(id)
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, s := range steps {
		if s.Step != "页面快照" {
			continue
		}
		var data struct {
			Files []string `json:"files"`
		}
		if err := json.Unmarshal([]byte(s.Data), &data); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, data.Files...)
	}
	return tokens
}

func TestTraceModeOwnsStepsAndSnapshots(t *testing.T) {
	const body = "<html><!--keep--><noscript>  原始字节 </noscript></html>"
	srv := serveBody(t, body)
	for _, mode := range []config.TraceMode{config.TraceOff, config.TraceErrors, config.TraceAll} {
		for _, outcome := range []string{"success", "failure", "warn", "panic", "cancel"} {
			t.Run(string(mode)+"/"+outcome, func(t *testing.T) {
				e, _, dir := snapshotEngine(t, mode, srv.URL)
				task := &Task{ID: 7, Site: "s", Stage: "stub", URL: srv.URL}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f := &funcFetcher{fn: func(ctx context.Context, task *Task, e *Engine) error {
					if _, err := e.FetchHTML(ctx, srv.URL); err != nil {
						return err
					}
					switch outcome {
					case "failure":
						return errors.New("failed")
					case "warn":
						task.Trace.Warn("degraded", errors.New("optional"), nil)
					case "panic":
						panic("original")
					case "cancel":
						cancel()
						return ctx.Err()
					}
					return nil
				}}
				func() {
					defer func() {
						if r := recover(); r != nil && r != "original" {
							t.Errorf("panic changed: %v", r)
						}
					}()
					_ = e.runAttempt(ctx, f, task, 0)
				}()
				keep := mode == config.TraceAll || mode == config.TraceErrors && (outcome == "failure" || outcome == "warn" || outcome == "panic")
				files := listEvidenceFiles(t, dir)
				if !keep {
					if len(files) != 0 {
						t.Fatalf("discarded attempt wrote %v", files)
					}
					return
				}
				tokens := snapshotTokens(t, e, task.ID)
				if len(tokens) != 1 || len(files) != 1 {
					t.Fatalf("tokens=%v files=%v", tokens, files)
				}
				got, err := e.TraceSnapshot(context.Background(), tokens[0])
				if err != nil || string(got) != body {
					t.Fatalf("snapshot=%q err=%v", got, err)
				}
				// Reopening the same trace root also recovers snapshot lookup.
				e.traces.files = tracestore.New(tracestore.Options{Dir: dir})
				got, err = e.TraceSnapshot(context.Background(), tokens[0])
				if err != nil || string(got) != body {
					t.Fatal("snapshot restart failed", err)
				}
				if err = e.traces.files.Cleanup(context.Background(), time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err = e.TraceSnapshot(context.Background(), tokens[0]); err == nil {
					t.Fatal("snapshot survived trace cleanup")
				}
			})
		}
	}
}

type funcFetcher struct {
	fn func(context.Context, *Task, *Engine) error
}

func (*funcFetcher) GetStage() string { return "stub" }
func (f *funcFetcher) FetchHandler(ctx context.Context, t *Task, e *Engine) error {
	return f.fn(ctx, t, e)
}

func TestTraceHTTPJSONFailureSnapshot(t *testing.T) {
	const body = `{"error":"temporary failure","items":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	e, _, _ := snapshotEngine(t, config.TraceErrors, srv.URL)
	f := &funcFetcher{fn: func(ctx context.Context, _ *Task, e *Engine) error {
		_, err := e.FetchHTTP(ctx, htmlfetch.Request{URL: srv.URL})
		return err
	}}
	if err := e.runAttempt(context.Background(), f, &Task{ID: 7, Site: "s", Stage: "stub", URL: srv.URL}, 0); err == nil {
		t.Fatal("expected HTTP failure")
	}
	tokens := snapshotTokens(t, e, 7)
	if len(tokens) != 1 {
		t.Fatal(tokens)
	}
	got, err := e.TraceSnapshot(context.Background(), tokens[0])
	if err != nil || string(got) != body {
		t.Fatal("JSON body lost", err)
	}
}

func TestTraceHTMLStatusFailureSnapshot(t *testing.T) {
	const body = "<html>missing page</html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	e, _, _ := snapshotEngine(t, config.TraceErrors, srv.URL)
	f := &funcFetcher{fn: func(ctx context.Context, _ *Task, e *Engine) error { _, err := e.FetchHTML(ctx, srv.URL); return err }}
	if err := e.runAttempt(context.Background(), f, &Task{ID: 7, Site: "s", Stage: "stub", URL: srv.URL}, 0); err == nil {
		t.Fatal("expected HTTP failure")
	}
	tokens := snapshotTokens(t, e, 7)
	if len(tokens) != 1 {
		t.Fatal(tokens)
	}
	got, err := e.TraceSnapshot(context.Background(), tokens[0])
	if err != nil || string(got) != body {
		t.Fatal("HTML error body lost", err)
	}
}

func TestTraceSnapshotConcurrentBoundedTail(t *testing.T) {
	tr := newTrace(nil, 7, 0)
	tr.snapshotLimit = 1024
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.holdRendered("<html>body</html>", fmt.Sprint(i))
			tr.Step("page", nil)
		}(i)
	}
	wg.Wait()
	tr.holdRendered("last page", "last")
	if len(tr.snapshots) > maxTraceSnapshots || tr.snapshotBytes > 1024 || !tr.truncated || tr.snapshots[len(tr.snapshots)-1].URL != "last" {
		t.Fatal("unbounded or wrong tail")
	}
	tr.setResult(errors.New("failure"))
	_ = tr.flush()
	before := len(tr.snapshots)
	tr.holdRendered("late", "late")
	if len(tr.snapshots) != before {
		t.Fatal("closed trace accepted body")
	}
}

func TestTraceSnapshotRejectsLargeStringBeforeAllocation(t *testing.T) {
	tr := newTrace(nil, 7, 0)
	tr.snapshotLimit = 16
	body := strings.Repeat("x", 1<<20)
	if allocations := testing.AllocsPerRun(10, func() { tr.holdRendered(body, "https://example.test") }); allocations != 0 {
		t.Fatalf("oversized snapshot allocated body copies: %v allocations", allocations)
	}
	if len(tr.snapshots) != 0 || !tr.truncated {
		t.Fatal("oversized snapshot was not rejected")
	}
}

func TestTraceSnapshotDoesNotChangeBusinessOutcomeOnQuotaFailure(t *testing.T) {
	srv := serveBody(t, "<html>snapshot</html>")
	e, _, _ := snapshotEngine(t, config.TraceAll, srv.URL)
	e.cfg.Crawler.TraceFiles.MaxTotalBytes = 1
	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, &Task{ID: 7, Stage: "stub", URL: srv.URL}, 0); err != nil {
		t.Fatalf("evidence changed business success: %v", err)
	}
}
