package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
)

func TestBreakerCoordinatorCoverageRetryDoesNotReuseProof(t *testing.T) {
	b := &breakerCoordinator{}
	b.SetSiteBreaker("fixture", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 3})
	s := b.smartBreakers["fixture"]
	now := time.Now()
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "fixture", State: "manual", Checks: []breaker.StageCheck{{Stage: "detail", TaskSuccesses: 3, TimedOutIDs: []string{"task:1"}}}}}
	s.event.MixedSamples = true
	s.event.Checks[0].Samples = []breaker.RecoverySample{{TaskID: 1, Successes: 3}}
	b.beginBreakerRetryLocked(s, now, "manual_retry", "retry", map[string]string{"detail": "complete detail coverage", "catalog": ""})
	if s.trial != 1 || s.event.Cycle != 1 || s.event.State != "cooldown" {
		t.Fatalf("retry did not advance control: %+v", s.event.Recovery)
	}
	if len(s.event.Checks) != 2 || s.event.Checks[0].Stage != "catalog" {
		t.Fatalf("coverage omitted registered stage: %+v", s.event.Checks)
	}
	detail := s.event.Checks[1]
	if detail.Verification != "readonly" || detail.TaskSuccesses != 0 || len(detail.TimedOutIDs) != 0 || detail.Samples[0].Successes != 0 {
		t.Fatalf("reused prior proof: %+v", detail)
	}
	if s.controlRevision <= s.savedRevision {
		t.Fatal("retry must be durable before admission")
	}
}

func TestBreakerCoordinatorProbeTimeoutRejectsLateHealthyResult(t *testing.T) {
	b := &breakerCoordinator{}
	b.SetSiteBreaker("fixture", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 3})
	s := b.smartBreakers["fixture"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "incident", Site: "fixture", State: "probing", Deadline: time.Now().Add(time.Minute), ProbeAttempts: 1, Checks: []breaker.StageCheck{{Stage: "detail", State: "probing", RequiredProbes: 1}}}}
	probeCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	token, reason := b.settleStageProbeLocked(BreakerProbeRequest{Site: "fixture", Stage: "detail"}, "incident", 1, BreakerProbeResult{Healthy: true, Semantic: true, Detail: "healthy"}, nil, probeCtx)
	if token != nil {
		reason = b.finishStageProbeLocked(token, context.Background())
	}
	if reason != "" || s.event.State != "manual" || s.event.Checks[0].ProbeSuccesses != 0 {
		t.Fatalf("late success accepted: %+v", s.event.Recovery)
	}
}

func TestBreakerStateAlgorithmsHaveNoEngineDependency(t *testing.T) {
	files, err := filepath.Glob("breaker_*_state.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == "Engine" {
				t.Error("breaker algorithms must accept value inputs, not Engine")
			}
			return true
		})
	}
}

func TestBreakerInternalMapsAreOnlyUsedByCoordinator(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			owner := false
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				if ptr, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := ptr.X.(*ast.Ident); ok {
						owner = id.Name == "breakerCoordinator"
					}
				}
			}
			if owner {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "smartBreakers" || sel.Sel.Name == "breakerProbes" || sel.Sel.Name == "breakerRuntime" || sel.Sel.Name == "breakerStore") {
					t.Errorf("%s: %s accesses private breaker state", file, fn.Name.Name)
				}
				return true
			})
		}
	}
}

func TestBreakerSampleReservationRejectsReplacedIncident(t *testing.T) {
	b := &breakerCoordinator{}
	b.SetSiteBreaker("fixture", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 3})
	s := b.smartBreakers["fixture"]
	s.event = &breakerEvent{Recovery: breaker.Recovery{ID: "old", Site: "fixture", State: "observing", MixedSamples: true, Deadline: time.Now().Add(time.Minute), SampleID: "task:7", SampleStage: "detail", SampleDeadline: time.Now().Add(time.Minute)}}
	token := b.reserveSampleLocked("fixture", false)
	if token == nil || !s.sampleDispatching {
		t.Fatal("did not reserve sample before I/O")
	}
	replacement := &breakerEvent{Recovery: breaker.Recovery{ID: "new", Site: "fixture", State: "observing", Deadline: time.Now().Add(time.Minute)}}
	s.event = replacement
	if b.sampleValidLocked(token, false) {
		t.Fatal("old I/O reservation accepted for new incident")
	}
	b.finishSampleLocked(token, true, nil)
	if s.sampleDispatching || replacement.Revision != 0 || len(replacement.pendingHistory) != 0 {
		t.Fatal("stale I/O result mutated replacement incident")
	}
}
