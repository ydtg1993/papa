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
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/account"
	"github.com/ydtg1993/papa/v3/pkg/browser"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"github.com/ydtg1993/papa/v3/pkg/discovery"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
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
	quality := ruleQualityFrom(ctx)
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
			quality.phase("extract", rulequality.FailureExtraction)
			out.Result, err = compiled.EvaluateRendered(out.SourceURL, body)
			if quality != nil {
				quality.evaluated(compiled.Rule(), out.Result, err)
			}
			recordRuleSnapshotValidation(ctx, out.Result.Validation)
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
			quality.phase("extract", rulequality.FailureExtraction)
			out.Result, err = compiled.Evaluate(response)
			if quality != nil {
				quality.evaluated(compiled.Rule(), out.Result, err)
			}
			recordRuleSnapshotValidation(ctx, out.Result.Validation)
			if err != nil {
				err = core.WrapNoRetryKind("rule-extraction", ruleSourceFailure("HTTP extraction", err))
			}
		}
	}
	// Keep a failed evaluation's internal report and original body identity.
	// Run returns on error before deduplication, planning or the business Save.
	if body != nil {
		digest := sha256.Sum256(body)
		out.BodySHA256 = hex.EncodeToString(digest[:])
	}
	if err != nil {
		return out, err
	}
	if out.Result.Truncated || out.Result.Validation.Truncated {
		return out, ruleSourceInvalid("extraction or validation exceeded its budget")
	}
	out.Rule = RuleRef{ID: task.Meta["rule_id"], Version: task.Meta["rule_version"]}
	return out, nil
}

func (f *RuleFetcher) readRuleHTTP(ctx context.Context, task *Task, e *Engine, req RuleRequest) (*htmlfetch.Response, error) {
	request := htmlfetch.Request{URL: req.URL, Method: http.MethodGet}
	if f.config.BuildRequest != nil {
		var err error
		request, err = f.config.BuildRequest(ctx, cloneRuleTask(task), req)
		if err != nil {
			ruleQualityFrom(ctx).phase("source", rulequality.FailurePlanning)
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
			ruleQualityFrom(ctx).phase("source", rulequality.FailureHost)
			return nil, ruleSourceInvalid("account manager is not configured")
		}
		response, err = e.FetchAccountHTTP(ctx, account.Key{Site: task.Site, ID: id}, request)
	} else {
		if e.htmlClient == nil {
			ruleQualityFrom(ctx).phase("source", rulequality.FailureHost)
			return nil, ruleSourceInvalid("HTTP client is not configured")
		}
		response, err = e.FetchHTTP(ctx, request)
	}
	if response != nil {
		// Capture status and complete-body identity even for rejected responses.
		// No URL, credentials, headers or body bytes enter the quality record.
		ruleQualityFrom(ctx).response(response.StatusCode, false, response.Body)
	}
	var oversized *htmlfetch.BodyTooLargeError
	if errors.As(err, &oversized) {
		ruleQualityFrom(ctx).phase("source", rulequality.FailureBudget)
		// Preserve the existing budget error precedence, but never attach even
		// unavailable-response metadata to an unvalidated final URL.
		if response != nil && response.URL != nil {
			if finalURL, scopeErr := f.allowedURL(response.URL.String()); scopeErr == nil && finalURL == canonical {
				recordRuleSnapshotUnavailable(ctx, finalURL, response.StatusCode, false, "response-body-too-large")
			}
		}
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
		// htmlfetch.Do assigns Body only after a complete read. A successful
		// empty response has a non-nil empty slice; nil means a failed/partial
		// read, including when account classification masks the transport error.
		// Record before status/account checks so rejected responses retain their
		// actual failure page without issuing another request.
		if response.Body == nil {
			recordRuleSnapshotUnavailable(ctx, finalURL, response.StatusCode, false, "response-body-unavailable")
		} else {
			recordRuleSnapshot(ctx, finalURL, response.StatusCode, false, response.Body)
		}
	}
	// The account manager's explicit classification takes precedence over the
	// wire status. In particular an expired POST is intentionally not replayed
	// inside Manager.Do; a replay-safe task must be able to refresh next attempt.
	for _, outcome := range []error{account.ErrUnknown, account.ErrPaused, account.ErrExpired, account.ErrForbidden, account.ErrChallenge, account.ErrSuperseded, account.ErrUnsupported} {
		if errors.Is(err, outcome) {
			ruleQualitySourceFailure(ctx, err, 0, rulequality.FailureNetwork)
			return nil, ruleSourceTransportError("account HTTP exchange", err)
		}
	}
	if response != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			ruleQualitySourceFailure(ctx, err, response.StatusCode, rulequality.FailureHTTP)
			statusErr := fmt.Errorf("rule fetcher: HTTP status %d", response.StatusCode)
			if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests {
				return nil, statusErr
			}
			return nil, core.WrapNoRetryKind("rule-source", statusErr)
		}
	}
	if err != nil {
		ruleQualitySourceFailure(ctx, err, 0, rulequality.FailureNetwork)
		return nil, ruleSourceTransportError("HTTP exchange", err)
	}
	if response == nil {
		ruleQualityFrom(ctx).phase("source", rulequality.FailureHost)
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
		if !discovery.SameOrigin(rawURL, finalURL) {
			// WithStatePage previously checked origin after the selector wait.
			// Keep that boundary after moving the wait into its callback, and
			// do not retain an account's cross-origin DOM before its manager
			// performs its own post-callback origin check.
			return ruleSourceFailure("browser origin check", errors.New("page state origin mismatch"))
		}
		html, err := page.HTML()
		if err != nil {
			return ruleSourceFailure("browser DOM read", err)
		}
		body = []byte(html)
		ruleQualityFrom(ctx).response(0, true, body)
		recordRuleSnapshot(ctx, finalURL, 0, true, body)
		return nil
	}
	// A failed wait commonly leaves a useful captcha/error DOM. The caller's
	// context may already be canceled, so permit only a short independent read
	// before the page owner closes it. Evidence failures never replace the
	// original navigation/wait error or trust the requested URL as the final URL.
	readFailure := func(page *rod.Page) {
		if ruleObservationFrom(ctx) == nil && ruleQualityFrom(ctx) == nil {
			return
		}
		captureCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		page = page.Context(captureCtx)
		// Rod's Page.Info uses the original Browser context, even on a page
		// clone. Rebind its equivalent protocol call too after cancellation.
		info, infoErr := (proto.TargetGetTargetInfo{TargetID: page.TargetID}).Call(page.Browser().Context(captureCtx))
		if infoErr != nil {
			return
		}
		actualURL, scopeErr := f.allowedURL(info.TargetInfo.URL)
		if scopeErr != nil || !discovery.SameOrigin(rawURL, actualURL) {
			return
		}
		html, htmlErr := page.HTML()
		if htmlErr != nil {
			ruleQualityFrom(ctx).response(0, true, nil)
			recordRuleSnapshotUnavailable(ctx, actualURL, 0, true, "browser-dom-unavailable")
			return
		}
		ruleQualityFrom(ctx).response(0, true, []byte(html))
		recordRuleSnapshot(ctx, actualURL, 0, true, []byte(html))
	}
	var err error
	if id := task.Meta[ruleAccountMeta]; id != "" {
		if e.GetAccountManager() == nil {
			ruleQualityFrom(ctx).phase("source", rulequality.FailureHost)
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
				readFailure(page)
				return err
			}
			if f.config.WaitSelector != "" {
				if _, err := page.Element(f.config.WaitSelector); err != nil {
					readFailure(page)
					return err
				}
			}
			return read(page)
		})
	} else {
		if e.browserPool == nil {
			ruleQualityFrom(ctx).phase("source", rulequality.FailureHost)
			return "", nil, ruleSourceInvalid("browser pool is not configured")
		}
		opts := mergeBrowserOptions(ctx, browser.PageOptions{Timeout: 30 * time.Second})
		err = e.browserPool.WithIsolatedBrowser(ctx, func(b *browser.Browser) error {
			return b.WithStatePage(ctx, rawURL, nil, opts, func(page *rod.Page) error {
				// Run this wait while the page is still available to us. The
				// enclosing 30 s context retains the original overall deadline.
				started := time.Now()
				if f.config.WaitSelector != "" {
					if _, waitErr := page.Element(f.config.WaitSelector); waitErr != nil {
						elapsed := time.Since(started)
						readFailure(page)
						return &browser.WaitError{Stage: "selector", Err: fmt.Errorf("selector %q: %w", f.config.WaitSelector, waitErr), Elapsed: elapsed}
					}
				}
				return read(page)
			})
		})
	}
	if err != nil {
		ruleQualitySourceFailure(ctx, err, 0, rulequality.FailureRender)
		return "", nil, ruleSourceTransportError("browser navigation", err)
	}
	return finalURL, body, nil
}
