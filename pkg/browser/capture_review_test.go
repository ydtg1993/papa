package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
)

type captureReviewCDP struct {
	events               chan *cdp.Event
	seen                 chan struct{}
	navigated            chan struct{}
	releaseNavigation    chan struct{}
	cancel               context.CancelFunc
	mode                 string
	mu                   sync.Mutex
	runtimeCalls, closes int
	unbounded            bool
	helperBeforeNavigate bool
}

func (c *captureReviewCDP) Event() <-chan *cdp.Event { return c.events }

func (c *captureReviewCDP) Call(ctx context.Context, session, method string, _ interface{}) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch method {
	case "Target.createTarget":
		return []byte(`{"targetId":"capture-target"}`), nil
	case "Target.attachToTarget":
		return []byte(`{"sessionId":"capture-session"}`), nil
	case "Page.navigate":
		defer close(c.navigated)
		c.events <- &cdp.Event{SessionID: session, Method: "Network.requestWillBeSent", Params: json.RawMessage(`{"requestId":"request-1","request":{"url":"https://capture.test/no-match","method":"GET"},"type":"Document"}`)}
		select {
		case <-c.seen:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c.mode == "canceled" {
			c.cancel()
		}
		if c.mode == "wait-load" {
			select {
			case <-c.releaseNavigation:
				return []byte(`{"frameId":"capture-target"}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		<-ctx.Done() // Keep navigation active until capture's teardown cancels it.
		return nil, ctx.Err()
	case "DOM.getDocument", "DOM.getOuterHTML", "Page.close":
		deadline, bounded := ctx.Deadline()
		c.mu.Lock()
		c.unbounded = c.unbounded || !bounded || time.Until(deadline) > captureHTMLTimeout
		if method == "Page.close" {
			c.closes++
		}
		c.mu.Unlock()
		if method == "Page.close" {
			c.events <- &cdp.Event{Method: "Target.targetDestroyed", Params: json.RawMessage(`{"targetId":"capture-target"}`)}
			return []byte(`{}`), nil
		}
		if method == "DOM.getDocument" {
			if c.mode == "panic" {
				panic("evidence-reader-failure")
			}
			if c.mode == "missing-root" {
				return []byte(`{"root":null}`), nil
			}
			return []byte(`{"root":{"nodeId":1}}`), nil
		}
		return []byte(`{"outerHTML":"<html><body>capture evidence</body></html>"}`), nil
	}
	if strings.HasPrefix(method, "Runtime.") {
		c.mu.Lock()
		c.runtimeCalls++
		if c.mode == "wait-load" {
			select {
			case <-c.navigated:
			default:
				c.helperBeforeNavigate = true
			}
		}
		c.mu.Unlock()
		return nil, errors.New("JS helper cannot safely run during navigation")
	}
	return []byte(`{}`), nil
}

func TestCaptureRequestWaitLoadStartsAfterNavigationFinishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &captureReviewCDP{
		events: make(chan *cdp.Event, 16), seen: make(chan struct{}), navigated: make(chan struct{}),
		releaseNavigation: make(chan struct{}), cancel: cancel, mode: "wait-load",
	}
	defer close(client.events)
	controller := rod.New().Client(client).NoDefaultDevice().Context(ctx)
	if err := controller.Connect(); err != nil {
		t.Fatal(err)
	}
	b := &Browser{Browser: controller, isolatedHeaders: map[string]string{}}
	type outcome struct {
		result *CaptureResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := b.CaptureRequest(ctx, "https://capture.test/page", CaptureOptions{
			Match:    func(CapturedRequest) bool { close(client.seen); return true },
			WaitLoad: true, CaptureTimeout: time.Second, Page: PageOptions{Timeout: time.Second},
		})
		finished <- outcome{result, err}
	}()
	<-client.seen
	// Hold the navigation response after the matching request has arrived. A
	// load waiter must not touch JS helpers until Navigate resets its context.
	var early *outcome
	select {
	case result := <-finished:
		early = &result
	case <-time.After(30 * time.Millisecond):
	}
	close(client.releaseNavigation)
	var result outcome
	if early != nil {
		result = *early
	} else {
		result = <-finished
	}
	if result.err != nil || result.result == nil || result.result.Request == nil {
		t.Fatalf("matched request lost: %+v %v", result.result, result.err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if early != nil || client.helperBeforeNavigate || client.runtimeCalls == 0 {
		t.Fatalf("load helper overlapped navigation: returned_early=%v helper_before_navigation=%v runtime_calls=%d", early != nil, client.helperBeforeNavigate, client.runtimeCalls)
	}
}

func TestCaptureRequestFailureEvidenceUsesIndependentDOMContext(t *testing.T) {
	for _, mode := range []string{"timeout", "canceled", "missing-root", "panic"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &captureReviewCDP{
				events: make(chan *cdp.Event, 16), seen: make(chan struct{}), navigated: make(chan struct{}), cancel: cancel, mode: mode,
			}
			browserCtx, stopBrowser := context.WithCancel(context.Background())
			defer stopBrowser()
			defer close(client.events)
			browser := rod.New().Client(client).NoDefaultDevice().Context(browserCtx)
			if err := browser.Connect(); err != nil {
				t.Fatal(err)
			}
			b := &Browser{Browser: browser, isolatedHeaders: map[string]string{}}
			res, err := b.CaptureRequest(ctx, "https://capture.test/page", CaptureOptions{
				Match:          func(CapturedRequest) bool { close(client.seen); return false },
				CaptureTimeout: 10 * time.Millisecond,
				Page:           PageOptions{Timeout: time.Second},
			})
			select {
			case <-client.navigated:
			case <-time.After(time.Second):
				t.Fatal("capture left navigation running")
			}
			cancel()
			if err == nil || mode == "canceled" && !errors.Is(err, context.Canceled) || mode != "canceled" && !strings.Contains(err.Error(), "no matching request") {
				t.Fatalf("original capture failure lost: %v", err)
			}
			if res == nil || res.SeenTotal != 1 || len(res.Seen) != 1 {
				t.Fatalf("failure request evidence lost: %+v", res)
			}
			if mode == "timeout" || mode == "canceled" {
				if !strings.Contains(res.HTML, "capture evidence") {
					t.Errorf("available HTML was lost after %s: %q", mode, res.HTML)
				}
			} else if res.HTML != "" {
				t.Errorf("unavailable document fabricated HTML: %q", res.HTML)
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if client.runtimeCalls != 0 || client.closes != 1 || client.unbounded {
				t.Errorf("unsafe evidence or cleanup: runtime_calls=%d closes=%d unbounded=%v", client.runtimeCalls, client.closes, client.unbounded)
			}
		})
	}
}
