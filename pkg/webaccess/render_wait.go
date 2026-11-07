package webaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v3/internal/selectorbudget"
)

var (
	ErrWaitInvalid = errors.New("webaccess: invalid render wait")
	ErrWaitTimeout = errors.New("webaccess: render wait timed out")
)

// RenderWait checks the first match in the main document, in an isolated world.
// Visible means a nonzero layout box with display/visibility permitting display;
// it does not assert viewport intersection, opacity, or absence of occlusion.
type RenderWait struct {
	Selector string
	State    string        // exists or visible
	Timeout  time.Duration // >0, <=30s, additionally bounded by the render context
}

// ValidateRenderWait rejects invalid options before any external work or charge.
func ValidateRenderWait(w RenderWait) error {
	if w.Selector == "" || selectorbudget.Validate(w.Selector) != nil || (w.State != "exists" && w.State != "visible") || w.Timeout <= 0 || w.Timeout > 30*time.Second {
		return ErrWaitInvalid
	}
	return nil
}

func waitRenderDOM(ctx context.Context, page *rod.Page, frame proto.PageFrameID, w RenderWait) error {
	waitCtx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	// Page.Context replaces Rod's native session context. Retain its target
	// destruction signal so a detached page cannot leave a CDP call pending
	// until the selector deadline. The outer render cause still takes priority.
	pageCtx := page.GetContext()
	stopPage := context.AfterFunc(pageCtx, cancel)
	defer stopPage()
	p := page.Context(waitCtx)
	selector, _ := json.Marshal(w.Selector)
	visible := w.State == "visible"
	// Only the selector is variable, encoded as a JS string literal. Page-world
	// replacements of querySelector/getComputedStyle cannot forge this result.
	expression := fmt.Sprintf(`(() => { const e = document.querySelector(%s); if (!e) return false; if (!%t) return true; const s = getComputedStyle(e), r = e.getBoundingClientRect(); return s.display !== 'none' && s.visibility !== 'hidden' && s.visibility !== 'collapse' && r.width > 0 && r.height > 0; })()`, selector, visible)
	classify := func(err error) error {
		if ctx.Err() != nil {
			return renderError(ctx, err)
		}
		if pageCtx.Err() != nil {
			return ErrRendererUnavailable
		}
		if waitCtx.Err() != nil {
			return ErrWaitTimeout
		}
		return renderError(ctx, err)
	}
	for {
		// Re-obtain the world after navigation; no world/session is retained
		// across Render calls. All CDP calls share the smaller deadline.
		world, err := (proto.PageCreateIsolatedWorld{FrameID: frame, WorldName: "papa-render-wait"}).Call(p)
		if err != nil {
			return classify(err)
		}
		result, err := (proto.RuntimeEvaluate{Expression: expression, ContextID: world.ExecutionContextID, ReturnByValue: true, Silent: true}).Call(p)
		if err != nil {
			return classify(err)
		}
		if result.ExceptionDetails != nil || result.Result == nil {
			return ErrWaitInvalid
		}
		if waitCtx.Err() != nil {
			return classify(waitCtx.Err())
		}
		if result.Result.Value.Bool() {
			return nil
		}
		if err := waitUntil(waitCtx, time.Now().Add(100*time.Millisecond)); err != nil {
			return classify(err)
		}
	}
}
