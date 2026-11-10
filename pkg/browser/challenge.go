package browser

import (
	"context"
	"net/http"
	"sync"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// siteChallengeObservation belongs to one managed navigation/callback scope.
// There is no global page map and no Fetch handler: independent Network events
// coexist with resource blocking, governance and state/identity initializers.
type siteChallengeObservation struct {
	mu       sync.Mutex
	err      error
	observed bool
}

// observeSiteChallenge subscribes synchronously before navigation. Its observer
// survives a navigation timeout long enough to preserve already-received
// protocol evidence, and cleanup cancels and joins it on every exit path.
func observeSiteChallenge(page *rod.Page) (*siteChallengeObservation, func()) {
	o := &siteChallengeObservation{}
	ctx, cancel := context.WithCancel(context.WithoutCancel(page.GetContext()))
	observer := page.Context(ctx)
	mainFrame := page.FrameID
	mainRequests := make(map[proto.NetworkRequestID]bool)
	var mainOrder []proto.NetworkRequestID
	wait := observer.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			if e.Type != proto.NetworkResourceTypeDocument || e.FrameID != mainFrame {
				return
			}
			if !mainRequests[e.RequestID] {
				mainRequests[e.RequestID] = true
				mainOrder = append(mainOrder, e.RequestID)
				if len(mainOrder) > 64 {
					delete(mainRequests, mainOrder[0])
					mainOrder = mainOrder[1:]
				}
			}
			if e.RedirectResponse != nil {
				o.record(e.RedirectResponse.Status, protocolResponseHeaders(e.RedirectResponse.Headers))
			}
		},
		func(e *proto.NetworkResponseReceivedExtraInfo) {
			// Headers can arrive before Chrome commits a document or emits
			// ResponseReceived (for example a stalled short response body).
			if mainRequests[e.RequestID] {
				o.record(e.StatusCode, protocolResponseHeaders(e.Headers))
			}
		},
		func(e *proto.NetworkResponseReceived) {
			if e.Type == proto.NetworkResourceTypeDocument && e.FrameID == mainFrame {
				o.record(e.Response.Status, protocolResponseHeaders(e.Response.Headers))
			}
		},
	)
	finished := make(chan struct{})
	go func() { defer close(finished); wait() }()
	var once sync.Once
	return o, func() { once.Do(func() { cancel(); <-finished }) }
}

func protocolResponseHeaders(headers proto.NetworkHeaders) http.Header {
	out := make(http.Header, len(headers))
	for key, value := range headers {
		out.Set(key, value.Str())
	}
	return out
}

func (o *siteChallengeObservation) record(status int, headers http.Header) {
	err := htmlfetch.DetectSiteChallenge(status, headers)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observed = true
	if err != nil && o.err == nil {
		o.err = err
	}
}

// prefer preserves explicit evidence over later load/selector/interaction
// errors, and over success. Unrelated third-party resources never reach record.
func (o *siteChallengeObservation) prefer(err error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	return err
}

func (o *siteChallengeObservation) responseObserved() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.observed
}
