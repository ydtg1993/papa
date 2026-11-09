package engine

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"net/url"
	"testing"
)

func TestRuleFetcherHostBrowserSourceBoundary(t *testing.T) {
	base := "https://example.test"
	u, _ := url.Parse(base + "/page")
	body := []byte("<article><b>title</b></article>")
	reg := crawlrule.NewRegistry()
	rule := crawlrule.Rule{SchemaVersion: 1, ID: "list", Version: "v1", Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "b"}}}
	if e := reg.Register(rule); e != nil {
		t.Fatal(e)
	}
	if _, e := reg.Activate("list", "v1", []crawlrule.Fixture{{Name: "dom", Response: &htmlfetch.Response{URL: u, StatusCode: 200, Body: body}, Expect: crawlrule.Expectations{RowCount: 1, Fields: []crawlrule.FieldExpectation{{Pointer: "/title", Value: "title"}}}}}); e != nil {
		t.Fatal(e)
	}
	cfg := ruleTestConfig(reg, base)
	cfg.Bindings = map[string]RuleRef{"list": {"list", "v1"}}
	cfg.ItemKey = "title"
	cfg.Transport = "browser"
	response := &htmlfetch.Response{URL: u, Body: body}
	cfg.BrowserSource = func(context.Context, string) (*htmlfetch.Response, error) { return response, nil }
	f := ruleTestFetcher(t, cfg)
	compiled, e := crawlrule.Compile(crawlrule.Rule{SchemaVersion: 1, ID: "dom", Version: "v1", Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "b"}}})
	if e != nil {
		t.Fatal(e)
	}
	task := &Task{Meta: map[string]string{}}
	page, e := f.readRulePage(context.Background(), task, &Engine{}, RuleRequest{URL: u.String()}, compiled)
	if e != nil || !page.Rendered || page.StatusCode != 0 || page.BodySHA256 == "" || len(page.Result.Records) != 1 {
		t.Fatalf("DOM extraction: %+v %v", page, e)
	}
	for _, kind := range []string{"status", "url", "nil", "account", "state"} {
		t.Run(kind, func(t *testing.T) {
			response = &htmlfetch.Response{URL: u, Body: body}
			task := &Task{Meta: map[string]string{}}
			req := RuleRequest{URL: u.String()}
			switch kind {
			case "status":
				response.StatusCode = 200
			case "url":
				response.URL, _ = url.Parse(base + "/different")
			case "nil":
				response.Body = nil
			case "account":
				task.Meta[ruleAccountMeta] = "account"
			case "state":
				req.State = "cursor"
			}
			if _, e := f.readRulePage(context.Background(), task, &Engine{}, req, compiled); e == nil {
				t.Fatal("invalid host response accepted")
			}
		})
	}
	cause := errors.New("sensitive provider response")
	f.config.BrowserSource = func(context.Context, string) (*htmlfetch.Response, error) { return nil, cause }
	if _, _, e := f.readRuleBrowserSource(context.Background(), task, u.String()); !errors.Is(e, cause) || e.Error() == cause.Error() {
		t.Fatal("error identity or safe message lost")
	}
	for _, kind := range []string{"http", "account", "wait"} {
		bad := cfg
		switch kind {
		case "http":
			bad.Transport = "http"
		case "account":
			bad.AccountID = "x"
		case "wait":
			bad.WaitSelector = "b"
		}
		if _, e := NewRuleFetcher(bad); e == nil {
			t.Fatal("incompatible host source accepted")
		}
	}
}
