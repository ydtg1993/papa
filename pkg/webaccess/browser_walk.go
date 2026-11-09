package webaccess

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

type browserWalk struct {
	plan   BrowserPlan
	result BrowseResult
}

// Browse retains one isolated browser across pages. All network work shares the
// Session budget. Partial evidence never implies complete extraction.
func (s *Session) Browse(ctx context.Context, raw string, plan BrowserPlan) (BrowseResult, error) {
	if plan.Validate() != nil {
		return BrowseResult{}, ErrBrowserPlan
	}
	b, _ := json.Marshal(plan)
	var frozen BrowserPlan
	if json.Unmarshal(b, &frozen) != nil {
		return BrowseResult{}, ErrBrowserPlan
	}
	w := &browserWalk{plan: frozen}
	opts := RenderOptions{}
	if frozen.Wait != nil {
		opts.Wait = &RenderWait{Selector: frozen.Wait.Selector, State: frozen.Wait.State, Timeout: time.Duration(frozen.Wait.TimeoutMS) * time.Millisecond}
	}
	_, _, err := s.render(ctx, raw, opts, w)
	return w.result, err
}

// evalBrowser runs only fixed host source in a new isolated world. Selectors and
// attributes are JSON literals, never concatenated executable model input.
func evalBrowser(ctx context.Context, p *rod.Page, frame proto.PageFrameID, expression string, out any) error {
	world, err := (proto.PageCreateIsolatedWorld{FrameID: frame, WorldName: "papa-browser-actions"}).Call(p.Context(ctx))
	if err != nil {
		return renderError(ctx, err)
	}
	r, err := (proto.RuntimeEvaluate{Expression: expression, ContextID: world.ExecutionContextID, ReturnByValue: true, Silent: true}).Call(p.Context(ctx))
	if err != nil {
		return renderError(ctx, err)
	}
	if r.ExceptionDetails != nil || r.Result == nil || r.Result.Value.Nil() {
		return ErrBrowserPlan
	}
	if json.Unmarshal([]byte(r.Result.Value.JSON("", "")), out) != nil {
		return ErrBrowserPlan
	}
	return nil
}
func browserLiteral(s string) string { b, _ := json.Marshal(s); return string(b) }

func browserKeys(ctx context.Context, p *rod.Page, frame proto.PageFrameID, scroll *BrowserScroll) (string, error) {
	var keys string
	err := evalBrowser(ctx, p, frame, fmt.Sprintf(`(() => {const nodes=document.querySelectorAll(%s); if(nodes.length>500)return null;const keys=Array.from(nodes,e=>%s==='text'?e.textContent:e.getAttribute(%s)); if(keys.some(k=>!k)||new Set(keys).size!==keys.length)return null;const value=JSON.stringify(keys);return value.length>65536?null:value;})()`, browserLiteral(scroll.ItemSelector), browserLiteral(scroll.KeyAttribute), browserLiteral(scroll.KeyAttribute)), &keys)
	return keys, err
}
func (s *Session) walkBrowser(ctx context.Context, p *rod.Page, frame proto.PageFrameID, w *browserWalk, snapshot func() (Response, error)) error {
	maxPages := 1
	if w.plan.Pagination != nil {
		maxPages = w.plan.Pagination.MaxPages
	}
	seen := map[[32]byte]bool{}
	visited := map[string]bool{}
	for pageIndex := 1; pageIndex <= maxPages; pageIndex++ {
		if pageIndex > 1 && w.plan.Wait != nil {
			v := w.plan.Wait
			if err := waitRenderDOM(ctx, p, frame, RenderWait{Selector: v.Selector, State: v.State, Timeout: time.Duration(v.TimeoutMS) * time.Millisecond}); err != nil {
				return err
			}
		}
		if scroll := w.plan.Scroll; scroll != nil {
			last, err := browserKeys(ctx, p, frame, scroll)
			if err != nil {
				return err
			}
			stable := 0
			stopped := false
			for round := 1; round <= scroll.MaxRounds; round++ {
				var ok bool
				if err = evalBrowser(ctx, p, frame, `(() => {const e=document.scrollingElement;if(!e)return false;window.scrollTo({top:e.scrollHeight,behavior:'instant'});return true;})()`, &ok); err != nil {
					return err
				}
				if !ok {
					return ErrBrowserPlan
				}
				if err = waitUntil(ctx, time.Now().Add(time.Duration(scroll.IntervalMS)*time.Millisecond)); err != nil {
					return renderError(ctx, err)
				}
				next, err := browserKeys(ctx, p, frame, scroll)
				if err != nil {
					return err
				}
				w.result.Steps = append(w.result.Steps, BrowserStep{pageIndex, "scroll", round})
				if !newBrowserKeys(last, next) {
					stable++
				} else {
					stable = 0
				}
				last = mergeBrowserKeys(last, next)
				if stable >= scroll.StableRounds {
					stopped = true
					break
				}
			}
			if !stopped {
				w.result.Truncated = true
				w.result.StopReason = "scroll_limit"
			}
		}
		response, err := snapshot()
		if err != nil {
			return err
		}
		hash := sha256.Sum256(response.Body)
		if seen[hash] {
			w.result.StopReason = "repeated_content"
			w.result.Truncated = true
			return nil
		}
		seen[hash] = true
		visited[response.URL] = true
		w.result.Pages = append(w.result.Pages, response)
		if w.result.Truncated {
			return nil
		}
		next := w.plan.Pagination
		if next == nil {
			w.result.StopReason = "complete"
			return nil
		}
		var target struct {
			Present bool   `json:"present"`
			URL     string `json:"url"`
		}
		probe := fmt.Sprintf(`(() => {const e=document.querySelector(%s);if(!e||e.disabled||e.getAttribute('aria-disabled')==='true')return {present:false,url:''};const r=e.getBoundingClientRect();if(!r.width||!r.height||getComputedStyle(e).visibility==='hidden')return {present:false,url:''};return {present:true,url:e.href||''};})()`, browserLiteral(next.Selector))
		if err = evalBrowser(ctx, p, frame, probe, &target); err != nil {
			return err
		}
		if !target.Present {
			w.result.StopReason = "no_next"
			return nil
		}
		if pageIndex == maxPages {
			w.result.StopReason = "page_limit"
			w.result.Truncated = true
			return nil
		}
		if next.Mode == "link" {
			canonical, _, targetErr := s.target(target.URL, false)
			if targetErr != nil {
				return targetErr
			}
			target.URL = canonical.String()
			if visited[target.URL] {
				w.result.StopReason = "repeated_url"
				w.result.Truncated = true
				return nil
			}
			if err = p.Context(ctx).Navigate(target.URL); err != nil {
				return renderError(ctx, err)
			}
			if err = p.Context(ctx).WaitLoad(); err != nil {
				return renderError(ctx, err)
			}
		} else {
			// A same-document button also consumes a logical page budget. A
			// subsequent document navigation may charge again, conservatively.
			if err = s.page(ctx); err != nil {
				return err
			}
			var clicked bool
			if err = evalBrowser(ctx, p, frame, fmt.Sprintf(`(() => {const e=document.querySelector(%s);if(!e||e.disabled)return false;e.click();return true;})()`, browserLiteral(next.Selector)), &clicked); err != nil {
				return err
			}
			if !clicked {
				return ErrBrowserPlan
			}
		}
		w.result.Steps = append(w.result.Steps, BrowserStep{pageIndex, "next_" + next.Mode, 1})
		if err = waitUntil(ctx, time.Now().Add(time.Duration(next.WaitMS)*time.Millisecond)); err != nil {
			return renderError(ctx, err)
		}
	}
	return nil
}

// Reordering existing items is not progress.
func newBrowserKeys(previous, next string) bool {
	var a, b []string
	_ = json.Unmarshal([]byte(previous), &a)
	_ = json.Unmarshal([]byte(next), &b)
	seen := map[string]bool{}
	for _, k := range a {
		seen[k] = true
	}
	for _, k := range b {
		if !seen[k] {
			return true
		}
	}
	return false
}

func mergeBrowserKeys(previous, next string) string {
	var a, b []string
	_ = json.Unmarshal([]byte(previous), &a)
	_ = json.Unmarshal([]byte(next), &b)
	seen := map[string]bool{}
	for _, k := range a {
		seen[k] = true
	}
	for _, k := range b {
		if !seen[k] {
			a = append(a, k)
			seen[k] = true
		}
	}
	data, _ := json.Marshal(a)
	return string(data)
}
