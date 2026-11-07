package crawlagent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

func TestHostRunIdentityCannotReplayModel(t *testing.T) {
	replay := replayActions(`{"tool":"fetch_page","url":"seed:0"}`)
	cfg, goal, _, source, audit := runnerHarness(t, replay)
	cfg.Budgets.MaxModelCalls = 1
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.RunWithID(context.Background(), "host-request-once", goal)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var budget, conflict int
	for err := range results {
		if errors.Is(err, crawlagent.ErrBudget) {
			budget++
		} else if errors.Is(err, crawlagent.ErrAudit) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if budget != 1 || conflict != 1 || len(replay.Calls()) != 1 || source.calls.Load() != 1 {
		t.Fatalf("duplicate run replayed: %d/%d calls=%d/%d", budget, conflict, len(replay.Calls()), source.calls.Load())
	}
	r, err := audit.Get(context.Background(), "host-request-once")
	if err != nil || r.Status != agentstore.Exhausted {
		t.Fatalf("audit=%+v err=%v", r, err)
	}
}

func TestHostRunIdentityRejectsUnsafeAndOversizedBeforeAudit(t *testing.T) {
	for _, id := range []string{"", "../escape", "with space", strings.Repeat("a", 65)} {
		replay := replayActions(`{"tool":"finish"}`)
		cfg, goal, _, _, _ := runnerHarness(t, replay)
		a, err := crawlagent.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.RunWithID(context.Background(), id, goal); !errors.Is(err, crawlagent.ErrInvalid) || len(replay.Calls()) != 0 {
			t.Fatalf("id %q: %v", id, err)
		}
	}
}

func TestHostEvidenceSamplesDoNotInventFixtureExpectations(t *testing.T) {
	replay := replayActions(candidateAction("b"), `{"tool":"finish"}`)
	cfg, goal, repo, source, _ := runnerHarness(t, replay)
	sample := addRunnerFixture(t, repo, "unused-host-set", "https://site.example/observation", "<article><b>PUBLIC</b></article>", "PUBLIC")
	goal.EvidenceSamples = []string{sample.ID}
	goal.Context = "Observed missing titles; investigate approved evidence."
	result, err := runAgent(t, cfg, goal)
	if err != nil || result.Status != agentstore.NeedsFixtures || result.Verified || source.calls.Load() != 0 || len(repo.verifications) != 0 {
		t.Fatalf("evidence became expectations: %+v %v", result, err)
	}
	if len(result.Evidence) != 1 || len(result.Candidates) != 1 {
		t.Fatalf("missing evidence/candidate: %+v", result)
	}
}

func TestHostEvidenceSamplesRemainSubjectToHoldoutIsolation(t *testing.T) {
	replay := replayActions(candidateAction("b"))
	cfg, goal, repo, _, _ := runnerHarness(t, replay)
	sample := addRunnerFixture(t, repo, "hidden", "https://site.example/hidden", "<article><b>HIDDEN</b></article>", "HIDDEN")
	goal.EvidenceSamples = []string{sample.ID}
	goal.Stages[0].HoldoutSet = "hidden"
	_, err := runAgent(t, cfg, goal)
	if err == nil || len(replay.Calls()) != 0 {
		t.Fatalf("hidden observation exposed: %v calls=%d", err, len(replay.Calls()))
	}
}

type panicCloseSource struct{ crawlagent.Source }

func (panicCloseSource) Close() error { panic("private cleanup detail") }

func TestHostRunCleanupPanicPreservesDurableResult(t *testing.T) {
	replay := replayActions(`{"tool":"fetch_page","url":"seed:0"}`)
	cfg, goal, _, source, audit := runnerHarness(t, replay)
	cfg.Budgets.MaxModelCalls = 1
	cfg.SourceFactory = func(webaccess.Config) (crawlagent.Source, error) { return panicCloseSource{source}, nil }
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.RunWithID(context.Background(), "cleanup-panic", goal)
	run, readErr := audit.Get(context.Background(), result.RunID)
	if !errors.Is(err, crawlagent.ErrBudget) || readErr != nil || result.Status != agentstore.Exhausted || run.Status != result.Status {
		t.Fatalf("cleanup lost durable outcome: result=%+v audit=%+v err=%v/%v", result, run, err, readErr)
	}
}

type canceledHostEvidence struct {
	crawlagent.Repository
	cancel context.CancelFunc
}

func (r canceledHostEvidence) Sample(ctx context.Context, id string) (rulestore.Sample, error) {
	r.cancel()
	return r.Repository.Sample(ctx, id)
}

func TestHostRunCancellationWhileLoadingEvidenceIsCanceled(t *testing.T) {
	for _, mode := range []string{"observation", "training", "holdout"} {
		t.Run(mode, func(t *testing.T) {
			replay := replayActions(`{"tool":"finish"}`)
			cfg, goal, repo, source, audit := runnerHarness(t, replay)
			sample := addRunnerFixture(t, repo, "fixtures", "https://site.example/observation", "<article><b>PUBLIC</b></article>", "PUBLIC")
			switch mode {
			case "observation":
				goal.EvidenceSamples = []string{sample.ID}
			case "training":
				goal.Stages[0].TrainingSet = "fixtures"
			case "holdout":
				goal.Stages[0].HoldoutSet = "fixtures"
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg.Repository = canceledHostEvidence{Repository: repo, cancel: cancel}
			a, err := crawlagent.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := a.RunWithID(ctx, "cancel-during-evidence", goal)
			run, readErr := audit.Get(context.Background(), result.RunID)
			if !errors.Is(err, context.Canceled) || readErr != nil || result.Status != agentstore.Canceled || run.Status != agentstore.Canceled || len(replay.Calls()) != 0 || source.calls.Load() != 0 {
				t.Fatalf("canceled evidence load recorded as failure: result=%+v audit=%+v err=%v/%v", result, run, err, readErr)
			}
		})
	}
}

type unconfirmedHostStart struct {
	agentstore.Store
	lost bool
}

func (s *unconfirmedHostStart) Start(ctx context.Context, run agentstore.Run) (agentstore.Run, error) {
	out, err := s.Store.Start(ctx, run)
	if err == nil && s.lost {
		return agentstore.Run{}, errors.New("injected lost Start commit response")
	}
	return out, err
}

func TestHostRunUnknownStartCommitNeverCallsModel(t *testing.T) {
	replay := replayActions(`{"tool":"finish"}`)
	cfg, goal, _, source, audit := runnerHarness(t, replay)
	uncertain := &unconfirmedHostStart{Store: audit, lost: true}
	cfg.Audit = uncertain
	a, err := crawlagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.RunWithID(context.Background(), "unconfirmed-start", goal)
	if !errors.Is(err, crawlagent.ErrAudit) || result.Status != agentstore.Interrupted || result.RunID != "unconfirmed-start" {
		t.Fatalf("unknown commit did not retain audit identity: %+v %v", result, err)
	}
	uncertain.lost = false
	_, err = a.RunWithID(context.Background(), result.RunID, goal)
	run, readErr := audit.Get(context.Background(), result.RunID)
	if !errors.Is(err, crawlagent.ErrAudit) || readErr != nil || run.Status != agentstore.Running || len(replay.Calls()) != 0 || source.calls.Load() != 0 {
		t.Fatalf("unknown Start authorized replay: audit=%+v err=%v/%v calls=%d", run, err, readErr, len(replay.Calls()))
	}
}
