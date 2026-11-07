package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

func snapshotPolicy() *htmlfetch.SamplePolicy {
	return &htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) { return body, nil }}
}

func readRuleObservations(t *testing.T, root string) []ruleSnapshotManifest {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, ruleSnapshotDir, "observations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out []ruleSnapshotManifest
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var m ruleSnapshotManifest
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}
func runSnapshotFixture(t *testing.T, mode string, policy *htmlfetch.SamplePolicy, saveErr error) (*Engine, *stubConnPool, string, string, error) {
	t.Helper()
	srv := serveBody(t, `{"rows":[{"id":"A"}]}`)
	e, trace, dir := archiveEngine(t, mode, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = policy
	id := ""
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error { id = p.EvidenceID; return saveErr }
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID = 71
	err := e.runAttempt(context.Background(), f, task, 0)
	return e, trace, dir, id, err
}

func TestRuleSnapshotLinksSaveTraceAndSingleBody(t *testing.T) {
	boom := errors.New("save unavailable")
	_, trace, dir, id, err := runSnapshotFixture(t, config.ArchiveModeFailure, snapshotPolicy(), boom)
	if !errors.Is(err, boom) {
		t.Fatalf("archive changed business error: %v", err)
	}
	observations := readRuleObservations(t, dir)
	if len(observations) != 1 || id == "" {
		t.Fatalf("missing observation/id: %d %s", len(observations), id)
	}
	m := observations[0]
	if m.EvidenceID != id || m.Rule.ID != "list" || m.Rule.Version != "v1" || m.TaskID != 71 || !m.Failed || m.BodyFile == "" {
		t.Fatalf("bad observation: %+v", m)
	}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m.BodyFile)))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"rows":[{"id":"A"}]}` || ruleDigest(body) != m.BodySHA256 || m.OriginalSHA256 != m.BodySHA256 {
		t.Fatal("stored body/digest differ")
	}
	if len(listArchive(t, dir)) != 2 {
		t.Fatalf("body copied into multiple artifacts: %v", listArchive(t, dir))
	}
	sql := trace.all()
	if !strings.Contains(sql, id) || !strings.Contains(sql, m.BodyFile) || strings.Contains(sql, `rows`) {
		t.Fatalf("Trace should reference body rather than copy it: %s", sql)
	}
}

func TestRuleSnapshotSuccessRetentionModes(t *testing.T) {
	for _, mode := range []string{config.ArchiveModeFailure, config.ArchiveModeAlways} {
		t.Run(mode, func(t *testing.T) {
			_, trace, dir, id, err := runSnapshotFixture(t, mode, snapshotPolicy(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if id == "" || !strings.Contains(trace.all(), id) {
				t.Fatal("success lost minimal evidence reference")
			}
			if mode == config.ArchiveModeFailure {
				if len(listArchive(t, dir)) != 0 || !strings.Contains(trace.all(), "not-retained") {
					t.Fatal("failure-only success falsely retained body")
				}
			} else {
				obs := readRuleObservations(t, dir)
				if len(obs) != 1 || obs[0].Failed || obs[0].BodyFile == "" || !strings.Contains(trace.all(), obs[0].BodyFile) {
					t.Fatal("always mode missing published reference")
				}
			}
		})
	}
}

func TestRuleSnapshotRetriesShareBodyKeepDistinctObservations(t *testing.T) {
	srv := serveBody(t, `{"rows":[{"id":"A"}]}`)
	e, trace, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = snapshotPolicy()
	boom := errors.New("save failed")
	ids := []string{}
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error { ids = append(ids, p.EvidenceID); return boom }
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID = 7
	for retry := 0; retry < 2; retry++ {
		task.Retry = retry
		if err := e.runAttempt(context.Background(), f, task, retry); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	}
	obs := readRuleObservations(t, dir)
	if len(obs) != 2 || len(ids) != 2 || ids[0] == ids[1] || obs[0].BodyFile != obs[1].BodyFile || len(listArchive(t, dir)) != 3 {
		t.Fatalf("retry dedup/observations: %+v", obs)
	}
	for _, id := range ids {
		if !strings.Contains(trace.all(), id) {
			t.Fatal("Trace missing observation")
		}
	}
}

func TestRuleSnapshotPolicyOmissionRedactionAndIsolation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy *htmlfetch.SamplePolicy
		reason string
	}{
		{"nil", nil, "body-policy-omitted"},
		{"body-error", &htmlfetch.SamplePolicy{RedactBody: func(bool, []byte) ([]byte, error) { return nil, errors.New("secret-redactor") }}, "body-redaction-failed"},
		{"body-panic", &htmlfetch.SamplePolicy{RedactBody: func(bool, []byte) ([]byte, error) { panic("secret-redactor") }}, "body-redaction-failed"},
		{"url-error", &htmlfetch.SamplePolicy{RedactURL: func(string) (string, error) { return "", errors.New("secret-url") }}, "url-redaction-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boom := errors.New("save failure")
			_, trace, dir, _, err := runSnapshotFixture(t, config.ArchiveModeFailure, tc.policy, boom)
			if !errors.Is(err, boom) {
				t.Fatalf("redactor changed task error: %v", err)
			}
			obs := readRuleObservations(t, dir)
			if len(obs) != 1 || obs[0].BodyOmitted != tc.reason || obs[0].BodyFile != "" || len(listArchive(t, dir)) != 1 {
				t.Fatalf("omission: %+v", obs)
			}
			for _, file := range listArchive(t, dir) {
				data, _ := os.ReadFile(filepath.Join(dir, file))
				if bytes.Contains(data, []byte("secret-")) {
					t.Fatal("redactor error leaked")
				}
			}
			if strings.Contains(trace.all(), "secret-") {
				t.Fatal("redactor error leaked to trace")
			}
		})
	}
	srv := serveBody(t, `{"rows":[{"id":"secret"}]}`)
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = &htmlfetch.SamplePolicy{QueryKeys: []string{"page"}, Headers: []string{"Cookie"}, RedactBody: func(request bool, b []byte) ([]byte, error) {
		if request {
			t.Fatal("request body passed to policy")
		}
		copy(b, bytes.ReplaceAll(b, []byte("secret"), []byte("masked")))
		return b, nil
	}}
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
		if p.Result.Records[0]["id"] != "secret" {
			t.Fatal("sanitizer mutated evaluated body")
		}
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	cfg.SnapshotPolicy.QueryKeys[0] = "token"
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL + "/?token=do-not-store&page=2"})
	task.ID = 7
	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatal(err)
	}
	obs := readRuleObservations(t, dir)
	if len(obs) != 1 {
		t.Fatal("missing redacted observation")
	}
	m := obs[0]
	if strings.Contains(m.SourceURL, "token") || !strings.Contains(m.SourceURL, "page=2") || m.BodySHA256 == m.OriginalSHA256 {
		t.Fatalf("redaction provenance: %+v", m)
	}
	b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m.BodyFile)))
	if !bytes.Contains(b, []byte("masked")) || bytes.Contains(b, []byte("secret")) {
		t.Fatal("redaction not applied")
	}
}

func TestRuleSnapshotWriteFailureDoesNotChangeTask(t *testing.T) {
	srv := serveBody(t, `{"rows":[{"id":"A"}]}`)
	e, trace, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	e.cfg.Crawler.Archive.Dir = blocked
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = snapshotPolicy()
	boom := errors.New("real save failure")
	cfg.Save = func(context.Context, *Task, RulePage) error { return boom }
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID = 7
	if err := e.runAttempt(context.Background(), f, task, 0); !errors.Is(err, boom) {
		t.Fatalf("write failure replaced cause: %v", err)
	}
	if !strings.Contains(trace.all(), "write-failed") || strings.Contains(trace.all(), `"files"`) {
		t.Fatal("Trace advertised unpublished files")
	}
}

func TestRuleSnapshotUnavailableWithoutAttemptOrDisabledArchive(t *testing.T) {
	srv := serveBody(t, `{"rows":[{"id":"A"}]}`)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = snapshotPolicy()
	cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
		if p.EvidenceID != "" {
			t.Fatal("invented reference outside archive context")
		}
		return nil
	}
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID = 7
	if _, err := ruleTestRun(t, f, task); err != nil {
		t.Fatal(err)
	}
	e, _, dir := archiveEngine(t, config.ArchiveModeAlways, srv.URL)
	e.cfg.Crawler.Archive.Enabled = false
	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatal(err)
	}
	if len(listArchive(t, dir)) != 0 {
		t.Fatal("disabled archive wrote files")
	}
}

func TestRuleSnapshotPanicStillPersistsReadablePage(t *testing.T) {
	srv := serveBody(t, `{"rows":[{"id":"A"}]}`)
	e, trace, dir := archiveEngine(t, config.ArchiveModeFailure, srv.URL)
	cfg := ruleTestConfig(ruleTestRegistry(t, srv.URL), srv.URL)
	cfg.SnapshotPolicy = snapshotPolicy()
	cfg.Save = func(context.Context, *Task, RulePage) error { panic("business panic") }
	f := ruleTestFetcher(t, cfg)
	task := ruleTestTask(t, f, RuleRequest{URL: srv.URL})
	task.ID = 7
	func() {
		defer func() {
			if recover() != "business panic" {
				t.Error("panic semantics changed")
			}
		}()
		_ = e.runAttempt(context.Background(), f, task, 0)
	}()
	obs := readRuleObservations(t, dir)
	if len(obs) != 1 || obs[0].BodyFile == "" || !obs[0].Failed || !strings.Contains(trace.all(), obs[0].EvidenceID) {
		t.Fatal("panic lost failure snapshot")
	}
}

func TestRuleSnapshotInputAndRedactedBodyBudgets(t *testing.T) {
	for _, outputLarge := range []bool{false, true} {
		t.Run(map[bool]string{false: "input", true: "output"}[outputLarge], func(t *testing.T) {
			e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
			e.cfg.Crawler.Archive.MaxFileMB = 1
			task := &Task{ID: 7, Site: "s", Stage: "list", Meta: map[string]string{"rule_id": "r", "rule_version": "v1"}}
			f := &RuleFetcher{config: RuleFetcherConfig{SnapshotPolicy: snapshotPolicy()}}
			input := []byte("ok")
			reason := "redacted-over-budget"
			if outputLarge {
				f.config.SnapshotPolicy.RedactBody = func(bool, []byte) ([]byte, error) { return make([]byte, (1<<20)+1), nil }
			} else {
				input = make([]byte, (1<<20)+1)
				reason = "input-over-budget"
			}
			ar := e.newArchiveBuffer(task, 0)
			ctx := f.beginRuleSnapshot(withArchive(context.Background(), ar), task, RuleRequest{URL: "https://example.test"}, "html")
			recordRuleSnapshot(ctx, "https://example.test", 200, false, input)
			ar.finish(true)
			obs := readRuleObservations(t, dir)
			if len(obs) != 1 || obs[0].BodyOmitted != reason || len(listArchive(t, dir)) != 1 {
				t.Fatalf("budget: %+v", obs)
			}
		})
	}
}

func TestArchiveCleanupPreservesLiveRuleSnapshotReferences(t *testing.T) {
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
	m := ruleSnapshotManifest{Version: 1, EvidenceID: strings.Repeat("a", 32), CreatedAt: time.Now()}
	first, oldFile, err := writeRuleSnapshot(dir, m, []byte("shared"))
	if err != nil {
		t.Fatal(err)
	}
	m.EvidenceID = strings.Repeat("b", 32)
	_, newFile, err := writeRuleSnapshot(dir, m, []byte("shared"))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, file := range []string{oldFile, first.BodyFile} {
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(file)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if removed := e.cleanupArchive(24 * time.Hour); removed != 1 {
		t.Fatalf("cleanup removed %d files; old body still has a live reference", removed)
	}
	for _, file := range []string{newFile, first.BodyFile} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(file))); err != nil {
			t.Fatalf("live artifact removed: %s: %v", file, err)
		}
	}
}

func TestRuleSnapshotObservationLimitAndTraceIndependence(t *testing.T) {
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
	e.cfg.Crawler.Trace.Enabled = false
	task := &Task{ID: 7, Site: "s", Stage: "list", Meta: map[string]string{"rule_id": "r", "rule_version": "v1"}}
	f := &RuleFetcher{config: RuleFetcherConfig{SnapshotPolicy: snapshotPolicy()}}
	ar := e.newArchiveBuffer(task, 0)
	base := withArchive(context.Background(), ar)
	for i := 0; i < maxArchivePagesPerAttempt+1; i++ {
		ctx := f.beginRuleSnapshot(base, task, RuleRequest{URL: "https://example.test"}, "html")
		recordRuleSnapshot(ctx, "https://example.test", 200, false, []byte("same"))
		if i == maxArchivePagesPerAttempt && ruleSnapshotID(ctx) != "" {
			t.Fatal("observation limit reused another ID")
		}
	}
	ar.finish(true)
	if got := len(readRuleObservations(t, dir)); got != maxArchivePagesPerAttempt {
		t.Fatalf("observations=%d", got)
	}
	if got := len(listArchive(t, dir)); got != maxArchivePagesPerAttempt+1 {
		t.Fatalf("dedup failed without Trace, files=%d", got)
	}
}

func TestRuleSnapshotNamespaceKeepsLegacySiteCleanup(t *testing.T) {
	e, _, dir := archiveEngine(t, config.ArchiveModeFailure)
	for _, site := range []string{"rule-snapshots", ".rule-snapshots"} {
		if sanitizePathSegment(site) == ruleSnapshotDir {
			t.Fatal("reserved namespace is reachable by legacy site sanitizer")
		}
		legacy := filepath.Join(dir, sanitizePathSegment(site), "observations", "task-7-try-0-00000000.html")
		if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(legacy, []byte("legacy"), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(legacy, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if removed := e.cleanupArchive(24 * time.Hour); removed != 2 {
		t.Fatalf("legacy site files no longer expire, removed=%d", removed)
	}
}
