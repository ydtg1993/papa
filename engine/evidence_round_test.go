package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
)

func TestArchiveExecutionIdentitySurvivesRetryReset(t *testing.T) {
	srv := serveBody(t, "first evidence")
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	task := &Task{ID: 9, Stage: "stub", URL: srv.URL}
	ids := map[string]bool{}
	for round := 0; round < 3; round++ {
		ctx := withEvidenceExecution(context.Background())
		id := evidenceExecutionID(ctx)
		if ids[id] {
			t.Fatal("claim identity reused")
		}
		ids[id] = true
		for attempt := 0; attempt < 2; attempt++ {
			task.Retry = attempt
			fetch := &pageFetcher{urls: []string{srv.URL}, err: errors.New("failure"), wrap: func(c context.Context) context.Context {
				tr, ar := traceFromCtx(c), archiveFromCtx(c)
				if tr.executionID != id || ar.executionID != id {
					t.Fatal("trace/archive identity mismatch")
				}
				return c
			}}
			_ = e.runAttempt(ctx, fetch, task, attempt)
		}
	}
	count := 0
	for _, rel := range listArchive(t, dir) {
		if !strings.HasSuffix(rel, ".json") {
			continue
		}
		count++
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		var meta map[string]any
		if err = json.Unmarshal(b, &meta); err != nil {
			t.Fatal(err)
		}
		id, _ := meta["execution_id"].(string)
		if !ids[id] || !strings.Contains(rel, id) {
			t.Fatalf("missing identity: %s", b)
		}
	}
	if count != 6 {
		t.Fatalf("retry reset overwrote evidence: %d", count)
	}
}

func TestListTraceLatestExecutionTailKeepsFailure(t *testing.T) {
	f := newFakeTaskDB()
	for i := 0; i < 1100; i++ {
		row := traceRow(int64(i+1), 7, 0, i, fmt.Sprint(i), int64(models.TraceOK), nil)
		row["execution_id"] = "old"
		if i >= 550 {
			row["execution_id"] = "new"
			row["attempt"] = int64(1)
			row["seq"] = int64(i - 550)
		}
		f.traceRows = append(f.traceRows, row)
	}
	f.traceRows[1099]["status"] = int64(models.TraceFailed)
	steps, err := traceDBEngine(t, f).ListTraceExecution(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 500 || !steps[0].Truncated || steps[0].Seq != 50 || steps[499].Status != "failed" {
		t.Fatalf("incorrect tail: len=%d first=%+v last=%+v", len(steps), steps[0], steps[len(steps)-1])
	}
	for i, s := range steps {
		if s.ExecutionID != "new" || s.Seq != i+50 {
			t.Fatalf("mixed execution/order: %+v", s)
		}
	}
}

func TestListTraceLegacyRoundsRemainInInsertionOrder(t *testing.T) {
	f := newFakeTaskDB()
	f.traceRows = []fakeRow{traceRow(3, 7, 0, 0, "third", 0, nil), traceRow(1, 7, 1, 0, "first", 0, nil), traceRow(2, 7, 0, 0, "second", 0, nil)}
	steps, err := traceDBEngine(t, f).ListTraceExecution(7)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"first", "second", "third"} {
		if steps[i].Step != want || steps[i].ExecutionID != "" {
			t.Fatalf("legacy order: %+v", steps)
		}
	}
}

type evidenceClaimFetcher struct {
	recordingFetcher
	ids []string
}

func (f *evidenceClaimFetcher) FetchHandler(ctx context.Context, task *Task, _ *Engine) error {
	f.ids = append(f.ids, evidenceExecutionID(ctx))
	task.Trace.Step("claim evidence", nil)
	if len(f.ids)%2 == 1 {
		return errors.New("retry once")
	}
	return nil
}

func TestEvidenceIdentityAllocatedByClaimAcrossEngineRecreation(t *testing.T) {
	seen := map[string]bool{}
	// Fresh engines model losing all process-local evidence state. Identity must
	// come from the successful claim path, not task ID/retry or a local counter.
	for round := 0; round < 3; round++ {
		e, _, task, _ := persistenceFixture(t)
		e.cfg.Crawler.Trace.Enabled = true
		fetcher := &evidenceClaimFetcher{}
		info := e.stages["stub:stub"]
		info.fetcher = fetcher
		info.config.MaxAttempts = 2
		info.config.Backoff = 0
		if err := e.stageHandler(info)(context.Background(), task); err != nil {
			t.Fatal(err)
		}
		if len(fetcher.ids) != 2 || fetcher.ids[0] == "" || fetcher.ids[0] != fetcher.ids[1] {
			t.Fatalf("retry lost claim identity: %v", fetcher.ids)
		}
		if seen[fetcher.ids[0]] {
			t.Fatal("recreated engine reused execution identity")
		}
		seen[fetcher.ids[0]] = true
	}
}
