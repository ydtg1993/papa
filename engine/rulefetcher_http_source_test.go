package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"net/http"
	"net/url"
	"testing"
)

func TestRuleFetcherControlledHTTPSource(t *testing.T) {
	const origin = "https://example.test"
	reg := ruleTestRegistry(t, origin)
	for _, test := range []string{"ok", "nil", "error", "redirect", "foreign", "missingURL"} {
		t.Run(test, func(t *testing.T) {
			cfg := ruleTestConfig(reg, origin)
			calls, saves := 0, 0
			cfg.Save = func(_ context.Context, _ *Task, p RulePage) error {
				saves++
				if p.BodySHA256 == "" || len(p.Result.Records) != 1 {
					t.Fatal("missing extraction or provenance")
				}
				return nil
			}
			cfg.HTTPSource = func(ctx context.Context, r htmlfetch.Request) (*htmlfetch.Response, error) {
				calls++
				if !r.NoRedirect || r.Method != http.MethodGet || r.URL != origin+"/page" {
					t.Fatalf("unsafe request %+v", r)
				}
				if test == "nil" {
					return nil, nil
				}
				if test == "error" {
					return nil, errors.New("source failed")
				}
				u, _ := url.Parse(r.URL)
				resp := &htmlfetch.Response{URL: u, StatusCode: 200, Body: []byte(`{"rows":[{"id":"one"}]}`)}
				if test == "redirect" {
					resp.StatusCode = 302
				}
				if test == "foreign" {
					resp.URL, _ = url.Parse("https://other.test/page")
				}
				if test == "missingURL" {
					resp.URL = nil
				}
				return resp, nil
			}
			f := ruleTestFetcher(t, cfg)
			task := ruleTestTask(t, f, RuleRequest{URL: origin + "/page"})
			_, err := ruleTestRun(t, f, task)
			if calls != 1 || ((err == nil) != (test == "ok")) || ((saves == 1) != (test == "ok")) {
				t.Fatalf("calls=%d saves=%d err=%v", calls, saves, err)
			}
		})
	}
	cfg := ruleTestConfig(reg, origin)
	cfg.HTTPSource = func(context.Context, htmlfetch.Request) (*htmlfetch.Response, error) {
		t.Fatal("account reached source")
		return nil, nil
	}
	cfg.AccountID = "account"
	if _, err := NewRuleFetcher(cfg); err == nil {
		t.Fatal("allowed account source")
	}
	cfg.AccountID = ""
	cfg.Transport = "browser"
	if _, err := NewRuleFetcher(cfg); err == nil {
		t.Fatal("allowed browser source")
	}
}
