package crawlagent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/agentstore"
	"github.com/ydtg1993/papa/v3/pkg/crawlagent"
	"github.com/ydtg1993/papa/v3/pkg/webaccess"
)

type waitRunnerSource struct {
	*runnerSource
	options []webaccess.RenderOptions
	timeout bool
}

func (s *waitRunnerSource) Render(ctx context.Context, raw string, opts webaccess.RenderOptions) (webaccess.Response, []webaccess.Response, error) {
	s.options = append(s.options, opts)
	if s.timeout && len(s.options) == 1 {
		return webaccess.Response{}, nil, webaccess.ErrWaitTimeout
	}
	return s.runnerSource.Render(ctx, raw, opts)
}

func TestRunnerRenderWaitForwardingAndAudit(t *testing.T) {
	const selector = `[data-value="private-selector-value"]`
	action := runnerJSON(map[string]any{"tool": "fetch_page", "url": "seed:0", "mode": "render", "wait": map[string]any{"selector": selector, "state": "visible", "timeout_ms": 3000}})
	for _, timeout := range []bool{false, true} {
		name := "success"
		if timeout {
			name = "timeout-can-correct"
		}
		t.Run(name, func(t *testing.T) {
			cfg, goal, _, base, audit := runnerHarness(t, replayActions(action, `{"tool":"fetch_page","url":"seed:0","mode":"render"}`))
			src := &waitRunnerSource{runnerSource: base, timeout: timeout}
			cfg.Web.Renderer.Enabled = true
			cfg.Budgets.MaxModelCalls = 2
			cfg.SourceFactory = func(c webaccess.Config) (crawlagent.Source, error) { base.config = c; return src, nil }
			result, err := runAgent(t, cfg, goal)
			if err == nil || result.Status != agentstore.Exhausted || result.Usage.ModelCalls != 2 || len(src.options) != 2 {
				t.Fatalf("bounded continuation failed: %+v err=%v options=%d", result, err, len(src.options))
			}
			first, last := src.options[0], src.options[1]
			if first.Wait == nil || first.Wait.Selector != selector || first.Wait.State != "visible" || first.Wait.Timeout != 3*time.Second || first.ObserveDuration != 0 || first.RecordAPI {
				t.Fatalf("wait forwarding: %+v", first)
			}
			if last.Wait != nil || last.ObserveDuration != 500*time.Millisecond {
				t.Fatalf("legacy behavior changed: %+v", last)
			}
			wantEvidence := 2
			if timeout {
				wantEvidence = 1
			}
			// Identical successful bodies may share a blob but retain per-fetch evidence.
			if len(result.Evidence) != wantEvidence || len(result.Candidates) != 0 {
				t.Fatalf("evidence=%d want=%d candidates=%d", len(result.Evidence), wantEvidence, len(result.Candidates))
			}
			steps, err := audit.Steps(context.Background(), result.RunID, 0)
			if err != nil || len(steps) != 4 {
				t.Fatalf("steps=%+v err=%v", steps, err)
			}
			raw := runnerJSON(steps)
			if strings.Contains(raw, "private-selector-value") {
				t.Fatal("raw selector leaked into audit")
			}
			hash := sha256.Sum256([]byte(selector))
			if !strings.Contains(steps[1].InputJSON, hex.EncodeToString(hash[:])) || !strings.Contains(steps[1].InputJSON, `"timeout_ms":3000`) {
				t.Fatalf("condition summary missing: %s", steps[1].InputJSON)
			}
			var out struct {
				Error string `json:"error"`
				Wait  struct {
					Elapsed int64  `json:"tool_elapsed_ms"`
					Outcome string `json:"outcome"`
				} `json:"render_wait"`
			}
			if err = json.Unmarshal([]byte(steps[1].OutputJSON), &out); err != nil {
				t.Fatal(err)
			}
			want := "succeeded"
			if timeout {
				want = "render_wait_timeout"
				if steps[1].Status != agentstore.StepFailed || out.Error != want {
					t.Fatalf("timeout audit: %+v", steps[1])
				}
			}
			if out.Wait.Outcome != want || out.Wait.Elapsed < 0 {
				t.Fatalf("wait summary: %+v", out)
			}
		})
	}
}

func TestRunnerRenderWaitCannotExpandPermission(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		render       bool
	}{
		{"renderer-off", `{"tool":"fetch_page","url":"seed:0","mode":"render","wait":{"selector":"#ready","state":"exists","timeout_ms":10}}`, false},
		{"http", `{"tool":"fetch_page","url":"seed:0","mode":"http","wait":{"selector":"#ready","state":"exists","timeout_ms":10}}`, true},
		{"record-api", `{"tool":"record_api","url":"seed:0","wait":{"selector":"#ready","state":"exists","timeout_ms":10}}`, true},
		{"excessive", `{"tool":"fetch_page","url":"seed:0","mode":"render","wait":{"selector":"#ready","state":"exists","timeout_ms":30001}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, goal, _, source, _ := runnerHarness(t, replayActions(tc.action))
			cfg.Web.Renderer.Enabled = tc.render
			cfg.Budgets.MaxModelCalls = 1
			result, err := runAgent(t, cfg, goal)
			if err == nil || source.calls.Load() != 0 || result.Usage.Pages != 0 || result.Usage.ModelCalls != 1 || len(result.Evidence) != 0 {
				t.Fatalf("invalid wait performed external work: %+v err=%v calls=%d", result, err, source.calls.Load())
			}
		})
	}
}
