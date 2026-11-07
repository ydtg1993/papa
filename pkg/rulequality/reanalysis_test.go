package rulequality_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent/model"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	q "github.com/ydtg1993/papa/v4/pkg/rulequality"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

type bridgeRules struct {
	crawlagent.Repository // unsupported methods fail loudly if unexpectedly used
	blobs                 *rulestore.FileBlobs
	mu                    sync.Mutex
	samples               map[string]rulestore.Sample
	rule                  crawlrule.Rule
	changed               bool
}

func (r *bridgeRules) Get(_ context.Context, ref rulestore.Reference) (rulestore.Version, error) {
	return rulestore.Version{Reference: ref, Rule: r.rule, EverPublished: true}, nil
}
func (r *bridgeRules) Snapshot(_ context.Context, site string) (crawlrule.ReleaseSnapshot, error) {
	version := "v1"
	if r.changed {
		version = "v2"
	}
	return crawlrule.ReleaseSnapshot{Site: site, ID: "release", Generation: 1, Bindings: map[string]crawlrule.Reference{"list": {ID: "titles", Version: version}}}, nil
}
func (r *bridgeRules) AddSample(ctx context.Context, in rulestore.SampleInput, p htmlfetch.SamplePolicy, a rulestore.Actor) (rulestore.Sample, error) {
	s, err := rulestore.SanitizeSample(in, p)
	if err != nil {
		return rulestore.Sample{}, err
	}
	if _, err = r.blobs.Put(ctx, s.Body); err != nil {
		return rulestore.Sample{}, err
	}
	s.Body = nil
	r.mu.Lock()
	defer r.mu.Unlock()
	id := fmt.Sprintf("sample-%d", len(r.samples)+1)
	out := rulestore.Sample{ID: id, SanitizedSample: s, Actor: a}
	r.samples[id] = out
	return out, nil
}
func (r *bridgeRules) Sample(_ context.Context, id string) (rulestore.Sample, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.samples[id]
	if !ok {
		return s, rulestore.ErrNotFound
	}
	return s, nil
}

func bridgeSnapshot(t *testing.T, dir, id string) {
	t.Helper()
	body := []byte("<article><b>APPROVED</b><i>PRIVATE</i></article>")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	rel := ".rule-snapshots/bodies/" + digest + ".body"
	for _, sub := range []string{"bodies", "observations"} {
		if err := os.MkdirAll(filepath.Join(dir, ".rule-snapshots", sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), body, 0600); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"version": 1, "evidence_id": id, "source_url": "https://quality.example/list?secret=PRIVATE", "status_code": 200, "rendered": false, "body_file": rel, "body_sha256": digest, "body_bytes": len(body), "original_sha256": strings.Repeat("c", 64)}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, ".rule-snapshots/observations", id+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func bridgeHarness(t *testing.T, mutate func(*q.ReanalysisConfig)) (*q.Reanalyzer, *q.MemoryStore, *agentstore.MemoryStore, *model.Replay, *bridgeRules, q.Request, q.ReanalysisInput) {
	t.Helper()
	ctx := context.Background()
	store := q.NewMemoryStore()
	audit := agentstore.NewMemoryStore()
	blobs, err := rulestore.NewFileBlobs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	compiled, err := crawlrule.Compile(crawlrule.Rule{ID: "titles", Version: "v1", SchemaVersion: 1, Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "b", Validation: crawlrule.Validation{Kind: "string", Required: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	rules := &bridgeRules{blobs: blobs, samples: map[string]rulestore.Sample{}, rule: compiled.Rule()}
	policy := htmlfetch.SamplePolicy{RedactBody: func(_ bool, body []byte) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(body), "PRIVATE", "[redacted]")), nil
	}}
	replay := model.NewReplay(model.ReplayStep{Reply: model.Reply{Content: `{"tool":"read_evidence","evidence_id":"latest"}`, Usage: model.Usage{InputTokens: 10, OutputTokens: 8, Known: true}}})
	actor := rulestore.Actor{Operator: "reviewer", Source: "quality reanalysis regression"}
	agent, err := crawlagent.New(crawlagent.Config{Model: replay, ModelName: "replay", Audit: audit, Repository: rules, Blobs: blobs, Actor: actor, SamplePolicy: policy, Web: webaccess.Config{Policy: webaccess.Policy{Origins: []webaccess.Origin{{URL: "https://quality.example"}}}}, Budgets: crawlagent.Budgets{MaxModelCalls: 1}})
	if err != nil {
		t.Fatal(err)
	}
	evidenceID := strings.Repeat("a", 32)
	dir := t.TempDir()
	bridgeSnapshot(t, dir, evidenceID)
	scope := q.Scope{Site: "quality", Stage: "list", RuleID: "titles", Version: "v1", ReleaseID: "release", ReleaseGeneration: 1}
	report := q.Report{ID: "report", Query: q.Query{Scope: scope, From: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour), Limit: 100}, Cutoff: 1, InputSHA256: strings.Repeat("b", 64), FieldSchemaSHA256: q.FieldSchemaDigest(rules.rule), Policy: q.Policy{MinPages: 1, MinRequests: 1}, Coverage: "recorded_observations_only", Folding: "latest_sequence_per_logical_key_or_observation_id", Verdict: q.SuspectedDrift, Metrics: q.Metrics{Attempts: 1, Pages: 1, DistinctRequests: 1, EvaluatedPages: 1, ExtractionFailures: 1}, Signals: []q.Signal{{Code: "extraction_failure_rate", Count: 1}}, Evidence: []q.EvidenceRef{{ObservationID: "observation", EvidenceID: evidenceID, BodySHA256: strings.Repeat("c", 64)}}}
	if _, err = store.SaveReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	cfg := q.ReanalysisConfig{Store: store, Agent: agent, Audit: audit, Rules: rules, ArchiveDir: dir, SamplePolicy: policy, Actor: actor}
	if mutate != nil {
		mutate(&cfg)
	}
	bridge, err := q.NewReanalyzer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := bridge.Prepare(ctx, "report", "operation")
	if err != nil {
		t.Fatal(err)
	}
	goal := crawlagent.Goal{Site: "quality", Seeds: []string{"https://quality.example/list"}, Stages: []crawlagent.StageGoal{{Stage: "list", RuleID: "titles", Fields: []crawlagent.FieldGoal{{Name: "title", Kind: "string", Required: true}}, Existing: &rulestore.Reference{ID: "titles", Version: "v1"}}}}
	return bridge, store, audit, replay, rules, request, q.ReanalysisInput{Goal: goal, EvidenceIDs: []string{evidenceID}}
}

func TestReanalysisConcurrentExecutionReservesOneModelRun(t *testing.T) {
	b, s, _, replay, _, request, input := bridgeHarness(t, nil)
	duplicate, err := b.Prepare(context.Background(), "report", "operation")
	if err != nil || duplicate.ID != request.ID || duplicate.RunID != request.RunID {
		t.Fatalf("duplicate request %+v %v", duplicate, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Execute(context.Background(), request.ID, request.Revision, input)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, crawlagent.ErrBudget) && !errors.Is(err, q.ErrConflict) && !errors.Is(err, q.ErrUnconfirmed) {
			t.Fatal(err)
		}
	}
	if len(replay.Calls()) != 1 {
		t.Fatalf("model calls=%d", len(replay.Calls()))
	}
	stored, err := s.Request(context.Background(), request.ID)
	if err != nil || stored.Status != q.Failed || stored.Revision != 3 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	transcript, _ := json.Marshal(replay.Calls())
	if strings.Contains(string(transcript), "PRIVATE") || !strings.Contains(string(transcript), "Runtime quality feedback") {
		t.Fatalf("unsafe or missing feedback: %s", transcript)
	}
	if strings.Contains(stored.ResultJSON, "https://") || strings.Contains(stored.ResultJSON, "PRIVATE") {
		t.Fatal("raw metadata duplicated into request")
	}
}

func TestReanalysisMissingEvidenceAndChangedScopeNeverCallModel(t *testing.T) {
	for _, mode := range []string{"none", "missing", "unselected", "goal", "published_changed", "snapshot_replaced"} {
		t.Run(mode, func(t *testing.T) {
			b, s, _, replay, rules, request, input := bridgeHarness(t, func(c *q.ReanalysisConfig) {
				if mode == "missing" {
					c.ArchiveDir = t.TempDir()
				}
				if mode == "snapshot_replaced" {
					path := filepath.Join(c.ArchiveDir, ".rule-snapshots", "observations", strings.Repeat("a", 32)+".json")
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					raw = []byte(strings.ReplaceAll(string(raw), strings.Repeat("c", 64), strings.Repeat("d", 64)))
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			})
			switch mode {
			case "none":
				input.EvidenceIDs = nil
			case "unselected":
				input.EvidenceIDs = []string{strings.Repeat("b", 32)}
			case "goal":
				input.Goal.Site = "another"
			case "published_changed":
				rules.changed = true
			}
			_, err := b.Execute(context.Background(), request.ID, request.Revision, input)
			if err == nil || len(replay.Calls()) != 0 {
				t.Fatalf("unexpected model: %v calls=%d", err, len(replay.Calls()))
			}
			stored, _ := s.Request(context.Background(), request.ID)
			if (mode == "none" || mode == "missing" || mode == "snapshot_replaced") && stored.Status != q.NeedsEvidence {
				t.Fatalf("missing evidence state=%s", stored.Status)
			}
		})
	}
}

type bridgeFailStore struct {
	q.Store
	start, end bool
}

func (s *bridgeFailStore) StartRequest(ctx context.Context, id string, rev uint64, digest string) (q.Request, error) {
	out, err := s.Store.StartRequest(ctx, id, rev, digest)
	if err == nil && s.start {
		return q.Request{}, errors.New("injected lost commit acknowledgment")
	}
	return out, err
}
func (s *bridgeFailStore) FinishRequest(ctx context.Context, id string, rev uint64, status, result string) (q.Request, error) {
	if s.end {
		return q.Request{}, errors.New("injected finish failure")
	}
	return s.Store.FinishRequest(ctx, id, rev, status, result)
}

func TestReanalysisUnknownStartIsReadOnlyOnRecovery(t *testing.T) {
	b, s, _, replay, _, request, input := bridgeHarness(t, func(c *q.ReanalysisConfig) { c.Store = &bridgeFailStore{Store: c.Store, start: true} })
	if _, err := b.Execute(context.Background(), request.ID, 1, input); !errors.Is(err, q.ErrUnconfirmed) {
		t.Fatal(err)
	}
	stored, _ := s.Request(context.Background(), request.ID)
	if stored.Status != q.Running || len(replay.Calls()) != 0 {
		t.Fatalf("reservation=%+v calls=%d", stored, len(replay.Calls()))
	}
	if _, err := b.Reconcile(context.Background(), request.ID, stored.Revision, false); !errors.Is(err, q.ErrInvalid) {
		t.Fatal(err)
	}
	out, err := b.Reconcile(context.Background(), request.ID, stored.Revision, true)
	if err != nil || out.Request.Status != q.Interrupted || len(replay.Calls()) != 0 {
		t.Fatalf("recovery=%+v err=%v", out, err)
	}
}

func TestReanalysisFinishFailureReconcilesWithoutAnotherModelCall(t *testing.T) {
	var fail *bridgeFailStore
	b, s, _, replay, _, request, input := bridgeHarness(t, func(c *q.ReanalysisConfig) { fail = &bridgeFailStore{Store: c.Store, end: true}; c.Store = fail })
	if _, err := b.Execute(context.Background(), request.ID, 1, input); !errors.Is(err, q.ErrUnconfirmed) {
		t.Fatal(err)
	}
	stored, _ := s.Request(context.Background(), request.ID)
	fail.end = false
	out, err := b.Reconcile(context.Background(), request.ID, stored.Revision, true)
	if err != nil || out.Request.Status != q.Failed || len(replay.Calls()) != 1 {
		t.Fatalf("recovery=%+v err=%v calls=%d", out, err, len(replay.Calls()))
	}
	if len(out.SampleIDs) != 1 || len(out.Gaps) != 1 || out.Gaps[0].Code != "budget_exhausted" {
		t.Fatalf("recovery lost imported samples or diagnosis: %+v", out)
	}
}

func TestCapabilityGapsDoNotInventInteractionTools(t *testing.T) {
	r := q.Report{Verdict: q.UpstreamFailure, Metrics: q.Metrics{FailureCounts: map[string]int{q.FailureAuthentication: 1, q.FailureChallenge: 1, q.FailureRender: 1}}}
	b, _ := json.Marshal(q.CapabilityGaps(r))
	if !strings.Contains(string(b), "needs_authentication") || strings.Contains(string(b), "needs_interaction") {
		t.Fatalf("gaps=%s", b)
	}
}

func TestReanalysisAcceptsImplicitFieldKind(t *testing.T) {
	b, _, _, replay, rules, request, input := bridgeHarness(t, nil)
	// HTML fields without an explicit kind still produce strings; the host
	// goal must name the resolved kind accepted by Agent, not an empty kind.
	rules.rule.Fields[0].Validation.Kind = ""
	_, err := b.Execute(context.Background(), request.ID, request.Revision, input)
	if len(replay.Calls()) != 1 {
		t.Fatalf("implicit HTML string kind blocked reanalysis: %v calls=%d", err, len(replay.Calls()))
	}
}

func TestReanalysisReconcileAcceptsFullAuditSnapshotBudget(t *testing.T) {
	b, s, audit, replay, _, request, _ := bridgeHarness(t, nil)
	started, err := s.StartRequest(context.Background(), request.ID, request.Revision, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := `{"Goal":{"evidence_samples":["approved"]},"Policy":{"padding":"` + strings.Repeat("x", 70<<10) + `"}}`
	run, err := audit.Start(context.Background(), agentstore.Run{ID: request.RunID, SnapshotJSON: snapshot, UsageJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	_, err = audit.Finish(context.Background(), run.ID, run.Revision, agentstore.Failed, `{}`, `{"status":"failed"}`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.Reconcile(context.Background(), request.ID, started.Revision, true)
	if err != nil || out.Request.Status != q.Failed || len(out.SampleIDs) != 1 || len(replay.Calls()) != 0 {
		t.Fatalf("valid audit snapshot cannot recover: %+v %v", out, err)
	}
}

func TestReanalysisCancellationAfterReservationDoesNotCallModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, s, _, replay, _, request, input := bridgeHarness(t, func(c *q.ReanalysisConfig) {
		c.SamplePolicy.RedactBody = func(_ bool, body []byte) ([]byte, error) { cancel(); return body, nil }
	})
	_, err := b.Execute(ctx, request.ID, request.Revision, input)
	stored, readErr := s.Request(context.Background(), request.ID)
	if !errors.Is(err, context.Canceled) || readErr != nil || stored.Status != q.Canceled || len(replay.Calls()) != 0 {
		t.Fatalf("canceled reservation: %+v err=%v/%v calls=%d", stored, err, readErr, len(replay.Calls()))
	}
}

func TestReanalysisUpstreamOnlyReportCannotPrepareRequest(t *testing.T) {
	b, s, _, replay, _, _, _ := bridgeHarness(t, nil)
	report, err := s.Report(context.Background(), "report")
	if err != nil {
		t.Fatal(err)
	}
	report.ID, report.CreatedAt, report.Verdict = "upstream", time.Time{}, q.UpstreamFailure
	if _, err = s.SaveReport(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Prepare(context.Background(), report.ID, "upstream-operation"); !errors.Is(err, q.ErrNotActionable) || len(replay.Calls()) != 0 {
		t.Fatalf("upstream report authorized reanalysis: %v", err)
	}
	if _, err = s.RequestByOperation(context.Background(), "upstream-operation"); !errors.Is(err, q.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestReanalysisInterruptPreservesImportedSampleReferences(t *testing.T) {
	b, s, audit, replay, _, request, _ := bridgeHarness(t, nil)
	ctx := context.Background()
	started, err := s.StartRequest(ctx, request.ID, request.Revision, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = audit.Start(ctx, agentstore.Run{ID: request.RunID, SnapshotJSON: `{"Goal":{"evidence_samples":["imported-sample"]}}`, UsageJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Reconcile(ctx, request.ID, started.Revision, true)
	run, readErr := audit.Get(ctx, request.RunID)
	if err != nil || readErr != nil || out.Request.Status != q.Interrupted || run.Status != agentstore.Interrupted || len(out.SampleIDs) != 1 || out.SampleIDs[0] != "imported-sample" || !strings.Contains(out.Request.ResultJSON, "imported-sample") || len(replay.Calls()) != 0 {
		t.Fatalf("interruption lost durable imported sample references: out=%+v audit=%+v err=%v/%v", out, run, err, readErr)
	}
}

type bridgeUnknownInterrupt struct{ agentstore.Store }

func (a bridgeUnknownInterrupt) Interrupt(ctx context.Context, id string, revision uint64) (agentstore.Run, error) {
	if _, err := a.Store.Interrupt(ctx, id, revision); err != nil {
		return agentstore.Run{}, err
	}
	return agentstore.Run{}, errors.New("injected lost Interrupt commit response")
}

func TestReanalysisUnknownInterruptCanReconcileWithoutModelReplay(t *testing.T) {
	b, s, audit, replay, _, request, _ := bridgeHarness(t, func(c *q.ReanalysisConfig) {
		c.Audit = bridgeUnknownInterrupt{Store: c.Audit}
	})
	ctx := context.Background()
	started, err := s.StartRequest(ctx, request.ID, request.Revision, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = audit.Start(ctx, agentstore.Run{ID: request.RunID, SnapshotJSON: `{"Goal":{"evidence_samples":["imported-sample"]}}`, UsageJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Reconcile(ctx, request.ID, started.Revision, true); !errors.Is(err, q.ErrUnconfirmed) {
		t.Fatalf("unknown interrupt was treated as confirmed: %v", err)
	}
	stored, err := s.Request(ctx, request.ID)
	if err != nil || stored.Status != q.Running || stored.Revision != started.Revision {
		t.Fatalf("unknown interrupt completed request: %+v %v", stored, err)
	}
	out, err := b.Reconcile(ctx, request.ID, stored.Revision, true)
	if err != nil || out.Request.Status != q.Interrupted || len(out.SampleIDs) != 1 || out.SampleIDs[0] != "imported-sample" || len(replay.Calls()) != 0 {
		t.Fatalf("unknown interruption could not recover references: %+v %v", out, err)
	}
}
