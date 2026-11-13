package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"net/http"
	"net/http/httptest"
	"testing"
)

type shutdownFetch struct {
	url, mode string
	warn      bool
}

func (f *shutdownFetch) GetStage() string { return "stub" }
func (f *shutdownFetch) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	if f.warn {
		task.Trace.Warn("preexisting", nil, nil)
	}
	if f.mode == "http" {
		_, err := e.FetchHTTP(ctx, htmlfetch.Request{URL: f.url})
		return err
	}
	_, err := e.FetchHTML(ctx, f.url)
	return err
}
func TestTraceShutdownTransportDoesNotCreateAnomaly(t *testing.T) {
	for _, mode := range []string{"http", "html"} {
		for _, warn := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "clean", true: "warning"}[warn], func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() }))
				defer srv.Close()
				e, _, _ := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
				err := e.runAttempt(ctx, &shutdownFetch{url: srv.URL, mode: mode, warn: warn}, &Task{ID: 7, Stage: "stub"}, 0)
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected cancellation: %v", err)
				}
				rows, listErr := e.ListTraceExecution(7)
				if listErr != nil || (len(rows) > 0) != warn {
					t.Fatalf("rows=%+v err=%v", rows, listErr)
				}
			})
		}
	}
}
func TestTraceShutdownCapturePreservesExplicitFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, explicit := range []bool{false, true} {
		tr := newTrace(nil, 7, 0)
		if explicit {
			tr.Fail("explicit", nil, nil)
		}
		e := &Engine{}
		e.recordCapture(withTrace(ctx, tr), "https://example.test", nil, context.Canceled)
		tr.setResult(nil)
		rows := tr.flush()
		if (len(rows) > 0) != explicit {
			t.Fatalf("explicit=%v rows=%+v", explicit, rows)
		}
	}
}
