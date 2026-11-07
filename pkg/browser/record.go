package browser

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// RecordOptions bounds a single page's network observation, including Interact.
// ObserveFor is the extra observation window after navigation/interaction.
type RecordOptions struct {
	Page               PageOptions
	Timeout            time.Duration
	ObserveFor         time.Duration
	MaxEntries         int
	MaxBodySize        int
	MaxTotalBodySize   int
	MaxRequestBodySize int
}

type NetworkEntry struct {
	CapturedResponse
	Completed   bool
	Redirect    bool
	BodyOmitted bool
	// Failure is a category, never the raw Chrome error or credential-bearing URL.
	Failure string
}

type NetworkRecording struct {
	Entries []NetworkEntry
	Dropped int
}

// WriteSample exports only complete, non-redirect entries with complete bodies.
func (e NetworkEntry) WriteSample(w io.Writer, policy htmlfetch.SamplePolicy) error {
	if !e.Completed || e.Redirect || e.Failure != "" || e.BodyOmitted || e.RequestBodyIncomplete {
		return fmt.Errorf("network entry is incomplete")
	}
	u, err := url.Parse(e.Request.URL)
	if err != nil {
		return err
	}
	h := map[string]string{}
	for k, v := range e.RequestHeader {
		h[k] = strings.Join(v, ", ")
	}
	return htmlfetch.WriteSample(w, htmlfetch.Request{URL: e.Request.URL, Method: e.Request.Method, Headers: h, Body: e.RequestBody}, &htmlfetch.Response{URL: u, StatusCode: e.StatusCode, Header: e.Header, Body: e.Body}, policy)
}

// RecordNetwork records the current page target, not all tabs, WebSocket frames
// or every worker. Raw data stays in memory. CDP may allocate full bodies before
// the returned-body limits can be checked. Incomplete entries remain explicit.
func (b *Browser) RecordNetwork(ctx context.Context, rawURL string, opts RecordOptions) (*NetworkRecording, error) {
	if err := validateNavigation(rawURL); err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.ObserveFor <= 0 {
		opts.ObserveFor = time.Second
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 128
	}
	if opts.MaxBodySize <= 0 {
		opts.MaxBodySize = 1 << 20
	}
	if opts.MaxTotalBodySize <= 0 {
		opts.MaxTotalBodySize = 8 << 20
	}
	if opts.MaxRequestBodySize <= 0 {
		opts.MaxRequestBodySize = 64 << 10
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	page, err := b.newPageBaseContext(ctx, opts.Page)
	if err != nil {
		return nil, err
	}
	defer func() { _ = page.Context(context.WithoutCancel(ctx)).Timeout(3 * time.Second).Close() }()
	eventsCtx, stop := context.WithCancel(ctx)
	defer stop()
	observer := page.Context(eventsCtx)
	record := &NetworkRecording{}
	active := map[proto.NetworkRequestID]int{}
	var mu sync.Mutex
	total := 0
	setResponse := func(entry *NetworkEntry, r *proto.NetworkResponse) {
		entry.StatusCode = r.Status
		entry.Header = make(http.Header)
		for k, v := range r.Headers {
			entry.Header.Set(k, v.Str())
		}
	}
	wait := observer.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			mu.Lock()
			defer mu.Unlock()
			if old, ok := active[e.RequestID]; ok && e.RedirectResponse != nil {
				entry := &record.Entries[old]
				setResponse(entry, e.RedirectResponse)
				entry.Redirect = true
				entry.Completed = true
				entry.BodyOmitted = true
				delete(active, e.RequestID)
			}
			if len(record.Entries) >= opts.MaxEntries {
				record.Dropped++
				return
			}
			r := NetworkEntry{CapturedResponse: CapturedResponse{Request: CapturedRequest{URL: e.Request.URL, Method: e.Request.Method, ResourceType: e.Type}, RequestHeader: make(http.Header)}, BodyOmitted: true}
			for k, v := range e.Request.Headers {
				r.RequestHeader.Set(k, v.Str())
			}
			raw := e.Request.PostData
			r.RequestBodyIncomplete = e.Request.HasPostData && raw == ""
			budget := min(opts.MaxRequestBodySize, max(0, opts.MaxTotalBodySize-total))
			if len(raw) > budget {
				raw = raw[:budget]
				r.RequestBodyIncomplete = true
			}
			r.RequestBody = []byte(raw)
			total += len(raw)
			active[e.RequestID] = len(record.Entries)
			record.Entries = append(record.Entries, r)
		},
		func(e *proto.NetworkResponseReceived) {
			mu.Lock()
			defer mu.Unlock()
			if i, ok := active[e.RequestID]; ok {
				setResponse(&record.Entries[i], e.Response)
			}
		},
		func(e *proto.NetworkLoadingFailed) {
			mu.Lock()
			defer mu.Unlock()
			if i, ok := active[e.RequestID]; ok {
				record.Entries[i].Completed = true
				record.Entries[i].Failure = "loading-failed"
				delete(active, e.RequestID)
			}
		},
		func(e *proto.NetworkLoadingFinished) {
			mu.Lock()
			i, ok := active[e.RequestID]
			if !ok {
				mu.Unlock()
				return
			}
			entry := &record.Entries[i]
			entry.Completed = true
			delete(active, e.RequestID)
			budget := min(opts.MaxBodySize, max(0, opts.MaxTotalBodySize-total))
			mu.Unlock()
			if budget == 0 {
				return
			}
			result, err := (proto.NetworkGetResponseBody{RequestID: e.RequestID}).Call(observer)
			var body []byte
			if err == nil {
				if result.Base64Encoded {
					body, err = base64.StdEncoding.DecodeString(result.Body)
				} else {
					body = []byte(result.Body)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			entry = &record.Entries[i]
			if err != nil {
				entry.Failure = "body-unavailable"
				return
			}
			if len(body) > budget {
				return
			}
			entry.Body = body
			entry.BodyOmitted = false
			total += len(body)
		},
	)
	finished := make(chan struct{})
	go func() { defer close(finished); wait() }()
	// Join the event consumer before handing its mutable recording to the caller.
	defer func() { stop(); <-finished }()
	navigation := page
	if opts.Page.Timeout > 0 {
		navigation = page.Timeout(opts.Page.Timeout)
	}
	err = navigation.Navigate(rawURL)
	if err == nil {
		err = navigation.WaitLoad()
	}
	if err == nil {
		err = applyWaits(navigation, opts.Page)
	}
	if err == nil && opts.Page.Interact != nil {
		err = opts.Page.Interact(ctx, page)
	}
	if err == nil {
		timer := time.NewTimer(opts.ObserveFor)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-timer.C:
		}
	}
	return record, err
}
