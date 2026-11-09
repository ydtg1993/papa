package crawlagent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
	"github.com/ydtg1993/papa/v4/pkg/webaccess"
)

type browserReviewSource struct {
	*runnerSource
	failure error
}

func (s *browserReviewSource) Browse(ctx context.Context, raw string, _ webaccess.BrowserPlan) (webaccess.BrowseResult, error) {
	s.usage.Pages = 1
	if e := s.config.OnRequest(ctx); e != nil {
		return webaccess.BrowseResult{}, e
	}
	s.usage.Requests = 1
	s.usage.Pages = 2 // same-document button: no second network request
	if e := s.config.OnBytes(ctx, int64(len(s.body))); e != nil {
		return webaccess.BrowseResult{}, e
	}
	s.usage.Bytes = int64(len(s.body))
	if s.failure != nil {
		return webaccess.BrowseResult{}, s.failure
	}
	return webaccess.BrowseResult{Pages: []webaccess.Response{{URL: raw, Rendered: true, Body: []byte(s.body)}}, StopReason: "no_next"}, nil
}
func TestRunnerBrowserSameDocumentPagesPersisted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{{"success", nil}, {"budget_failure", webaccess.ErrBudget}, {"canceled", webaccess.ErrCanceled}} {
		t.Run(tc.name, func(t *testing.T) {
			replay := replayActions(`{"tool":"fetch_page","stage":"catalog","mode":"render","url":"seed:0"}`)
			cfg, goal, _, source, audit := runnerHarness(t, replay)
			cfg.Budgets.MaxModelCalls = 1
			cfg.Web.Renderer.Enabled = true
			goal.Stages[0].Transport = "browser"
			goal.Stages[0].Browser = &webaccess.BrowserPlan{}
			cfg.SourceFactory = func(c webaccess.Config) (crawlagent.Source, error) {
				source.config = c
				return &browserReviewSource{source, tc.failure}, nil
			}
			out, _ := runAgent(t, cfg, goal)
			if out.Usage.Pages != 2 {
				t.Fatalf("same-document pages undercounted: got %d want 2", out.Usage.Pages)
			}
			run, e := audit.Get(context.Background(), out.RunID)
			if e != nil {
				t.Fatal(e)
			}
			var persisted crawlagent.Usage
			if e := json.Unmarshal([]byte(run.UsageJSON), &persisted); e != nil || persisted.Pages != 2 {
				t.Fatalf("usage not durable: %+v %v", persisted, e)
			}

		})
	}
}
