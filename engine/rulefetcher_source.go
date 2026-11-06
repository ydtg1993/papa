package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/account"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// ruleSourceError preserves error identity without copying URLs, provider
// messages, response fragments or credentials into task errors and traces.
type ruleSourceError struct {
	operation string
	cause     error
}

func (e *ruleSourceError) Error() string { return "rule fetcher: " + e.operation + " failed" }
func (e *ruleSourceError) Unwrap() error { return e.cause }

func ruleSourceFailure(operation string, cause error) error {
	return &ruleSourceError{operation: operation, cause: cause}
}

func ruleSourceInvalid(message string) error {
	return core.WrapNoRetryKind("rule-source", errors.New("rule fetcher: "+message))
}

func (f *RuleFetcher) readRulePage(ctx context.Context, task *Task, e *Engine, req RuleRequest, compiled *crawlrule.Compiled) (RulePage, error) {
	var out RulePage
	if task == nil || e == nil || compiled == nil {
		return out, ruleSourceInvalid("task, engine and compiled rule required")
	}
	requested, err := f.allowedURL(req.URL)
	if err != nil {
		return out, core.WrapNoRetryKind("rule-source", ruleSourceFailure("request scope validation", err))
	}
	req.URL = requested
	var body []byte
	if f.config.Transport == "browser" {
		if f.config.BuildRequest != nil || req.State != "" {
			return out, ruleSourceInvalid("browser transport supports URL navigation only")
		}
		out.SourceURL, body, err = f.readRuleBrowser(ctx, task, e, req.URL)
		out.Rendered = true
		if err == nil {
			out.Result, err = compiled.EvaluateRendered(out.SourceURL, body)
			if err != nil {
				err = core.WrapNoRetryKind("rule-extraction", ruleSourceFailure("rendered extraction", err))
			}
		}
	} else {
		var response *htmlfetch.Response
		response, err = f.readRuleHTTP(ctx, task, e, req)
		if err == nil {
			out.SourceURL = response.URL.String()
			out.StatusCode = response.StatusCode
			body = response.Body
			out.Result, err = compiled.Evaluate(response)
			if err != nil {
				err = core.WrapNoRetryKind("rule-extraction", ruleSourceFailure("HTTP extraction", err))
			}
		}
	}
	if err != nil {
		return RulePage{}, err
	}
	if out.Result.Truncated || out.Result.Validation.Truncated {
		return RulePage{}, ruleSourceInvalid("extraction or validation exceeded its budget")
	}
	out.Rule = RuleRef{ID: task.Meta["rule_id"], Version: task.Meta["rule_version"]}
	digest := sha256.Sum256(body)
	out.BodySHA256 = hex.EncodeToString(digest[:])
	return out, nil
}

func (f *RuleFetcher) readRuleHTTP(ctx context.Context, task *Task, e *Engine, req RuleRequest) (*htmlfetch.Response, error) {
	request := htmlfetch.Request{URL: req.URL, Method: http.MethodGet}
	if f.config.BuildRequest != nil {
		var err error
		request, err = f.config.BuildRequest(ctx, cloneRuleTask(task), req)
		if err != nil {
			return nil, ruleSourceFailure("request construction", err)
		}
	}
	canonical, err := f.allowedURL(request.URL)
	if err != nil || canonical != req.URL {
		return nil, ruleSourceInvalid("BuildRequest must preserve the requested URL")
	}
	request.URL = canonical
	if request.Method == "" {
		request.Method = http.MethodGet
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost:
		if !f.config.ReplaySafe {
			return nil, ruleSourceInvalid("POST requires ReplaySafe because task retries repeat requests")
		}
	default:
		return nil, ruleSourceInvalid("only GET, HEAD and explicitly replay-safe POST are supported")
	}
	// A redirect is rejected even when it targets another allowed origin. This
	// prevents request/context credentials from being forwarded before validation
	// and leaves the exact same policy in force for authenticated requests.
	request.NoRedirect = true
	var response *htmlfetch.Response
	if id := task.Meta[ruleAccountMeta]; id != "" {
		if e.GetAccountManager() == nil {
			return nil, ruleSourceInvalid("account manager is not configured")
		}
		response, err = e.FetchAccountHTTP(ctx, account.Key{Site: task.Site, ID: id}, request)
	} else {
		if e.htmlClient == nil {
			return nil, ruleSourceInvalid("HTTP client is not configured")
		}
		response, err = e.FetchHTTP(ctx, request)
	}
	var oversized *htmlfetch.BodyTooLargeError
	if errors.As(err, &oversized) {
		return nil, core.WrapNoRetryKind("rule-source", ruleSourceFailure("response body budget", err))
	}
	if response != nil {
		if response.URL == nil {
			return nil, ruleSourceInvalid("response final URL is missing")
		}
		finalURL, scopeErr := f.allowedURL(response.URL.String())
		if scopeErr != nil {
			return nil, ruleSourceInvalid("response final URL is outside configured origins")
		}
		if finalURL != canonical {
			return nil, ruleSourceInvalid("HTTP transport changed URL despite redirect prohibition")
		}
	}
	// The account manager's explicit classification takes precedence over the
	// wire status. In particular an expired POST is intentionally not replayed
	// inside Manager.Do; a replay-safe task must be able to refresh next attempt.
	for _, outcome := range []error{account.ErrUnknown, account.ErrPaused, account.ErrExpired, account.ErrForbidden, account.ErrChallenge, account.ErrSuperseded, account.ErrUnsupported} {
		if errors.Is(err, outcome) {
			return nil, ruleSourceTransportError("account HTTP exchange", err)
		}
	}
	if response != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			statusErr := fmt.Errorf("rule fetcher: HTTP status %d", response.StatusCode)
			if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests {
				return nil, statusErr
			}
			return nil, core.WrapNoRetryKind("rule-source", statusErr)
		}
	}
	if err != nil {
		return nil, ruleSourceTransportError("HTTP exchange", err)
	}
	if response == nil {
		return nil, ruleSourceInvalid("HTTP transport returned no response")
	}
	return response, nil
}

func ruleSourceTransportError(operation string, err error) error {
	safe := ruleSourceFailure(operation, err)
	for _, permanent := range []error{account.ErrUnknown, account.ErrPaused, account.ErrForbidden, account.ErrChallenge, account.ErrUnsupported} {
		if errors.Is(err, permanent) {
			return core.WrapNoRetryKind("rule-source", safe)
		}
	}
	return safe
}

func (f *RuleFetcher) readRuleBrowser(ctx context.Context, task *Task, e *Engine, rawURL string) (string, []byte, error) {
	if core.ProxyURLFrom(ctx) != "" {
		return "", nil, ruleSourceInvalid("browser transport requires a pool-bound proxy")
	}
	// Covers the callback too: WithStatePage bounds navigation separately and
	// returns a page bound to its caller's context for HTML and Info operations.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body []byte
	var finalURL string
	read := func(page *rod.Page) error {
		info, err := page.Info()
		if err != nil {
			return ruleSourceFailure("browser final URL", err)
		}
		finalURL, err = f.allowedURL(info.URL)
		if err != nil {
			return ruleSourceInvalid("browser final URL is outside configured origins")
		}
		html, err := page.HTML()
		if err != nil {
			return ruleSourceFailure("browser DOM read", err)
		}
		body = []byte(html)
		return nil
	}
	var err error
	if id := task.Meta[ruleAccountMeta]; id != "" {
		if e.GetAccountManager() == nil {
			return "", nil, ruleSourceInvalid("account manager is not configured")
		}
		err = e.WithAccountBrowser(ctx, account.Key{Site: task.Site, ID: id}, func(page *rod.Page) error {
			page = page.Context(ctx)
			// The manager owns account headers and browser identity. Check its
			// authenticated bootstrap origin before navigating with credentials.
			same, err := page.Eval(`(url) => location.origin === new URL(url).origin`, rawURL)
			if err != nil {
				return ruleSourceFailure("account browser origin check", err)
			}
			if !same.Value.Bool() {
				return ruleSourceInvalid("requested URL differs from account browser origin")
			}
			if err := page.Navigate(rawURL); err != nil {
				return err
			}
			if err := page.WaitLoad(); err != nil {
				return err
			}
			if f.config.WaitSelector != "" {
				if _, err := page.Element(f.config.WaitSelector); err != nil {
					return err
				}
			}
			return read(page)
		})
	} else {
		if e.browserPool == nil {
			return "", nil, ruleSourceInvalid("browser pool is not configured")
		}
		opts := mergeBrowserOptions(ctx, browser.PageOptions{WaitSelector: f.config.WaitSelector, Timeout: 30 * time.Second})
		err = e.browserPool.WithIsolatedBrowser(ctx, func(b *browser.Browser) error {
			return b.WithStatePage(ctx, rawURL, nil, opts, read)
		})
	}
	if err != nil {
		return "", nil, ruleSourceTransportError("browser navigation", err)
	}
	return finalURL, body, nil
}
