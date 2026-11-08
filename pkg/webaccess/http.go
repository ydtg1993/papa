package webaccess

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/temoto/robotstxt"
)

type originState struct {
	gate        chan struct{}
	checked     bool
	robots      *robotstxt.Group
	robotsError error
	delay       time.Duration
	next        time.Time
}

func (s *Session) state(key string) *originState {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.states[key]
	if v == nil {
		v = &originState{gate: make(chan struct{}, 1), delay: s.minInterval}
		s.states[key] = v
	}
	return v
}
func waitUntil(ctx context.Context, at time.Time) error {
	d := time.Until(at)
	if d <= 0 {
		if ctx.Err() != nil {
			return ErrCanceled
		}
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ErrCanceled
	case <-t.C:
		return nil
	}
}

func (s *Session) Get(ctx context.Context, raw string) (Response, error) {
	ctx, cancel := s.operation(ctx)
	defer cancel()
	if _, _, err := s.target(raw, false); err != nil {
		return Response{}, err
	}
	if err := s.page(ctx); err != nil {
		return Response{}, err
	}
	return s.follow(ctx, raw, false)
}

// GetNoRedirect applies the same origin, robots, rate and shared budgets as Get,
// but returns the first response (including 3xx) without requesting Location.
func (s *Session) GetNoRedirect(ctx context.Context, raw string) (Response, error) {
	ctx, cancel := s.operation(ctx)
	defer cancel()
	if _, _, err := s.target(raw, false); err != nil {
		return Response{}, err
	}
	if err := s.page(ctx); err != nil {
		return Response{}, err
	}
	response, _, err := s.fetch(ctx, raw, false)
	return response, err
}

func (s *Session) follow(ctx context.Context, raw string, resource bool) (Response, error) {
	for hops := 0; ; hops++ {
		resp, headers, err := s.fetch(ctx, raw, resource)
		if err != nil {
			return Response{}, err
		}
		if !redirect(resp.StatusCode) {
			return resp, nil
		}
		if hops >= s.budget.MaxRedirects {
			return Response{}, ErrBudget
		}
		u, _, err := s.target(raw, resource)
		if err != nil {
			return Response{}, err
		}
		next, err := u.Parse(headers.Get("Location"))
		if err != nil || headers.Get("Location") == "" {
			return Response{}, ErrTransport
		}
		raw = next.String()
		if _, _, err = s.target(raw, resource); err != nil {
			return Response{}, err
		}
	}
}

func redirect(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// fetch performs a guarded GET with optional robots checking. It is also the
// browser response backend: no browser headers, Cookie or auth are forwarded.
func (s *Session) fetch(ctx context.Context, raw string, resource bool) (Response, http.Header, error) {
	u, permission, err := s.target(raw, resource)
	if err != nil {
		return Response{}, nil, err
	}
	key := u.Scheme + "://" + u.Host
	st := s.state(key)
	select {
	case st.gate <- struct{}{}:
		defer func() { <-st.gate }()
	case <-ctx.Done():
		return Response{}, nil, ErrCanceled
	}
	if !s.respectRobots {
		return s.roundTrip(ctx, u, permission, st)
	}
	if !st.checked {
		st.checked = true
		robotsURL := *u
		robotsURL.Path = "/robots.txt"
		robotsURL.RawPath = ""
		robotsURL.RawQuery = ""
		r, _, fetchErr := s.roundTrip(ctx, &robotsURL, permission, st)
		if fetchErr != nil {
			st.robotsError = fetchErr
		} else if r.StatusCode == 404 || r.StatusCode == 410 {
			st.robots = &robotstxt.Group{}
		} else if r.StatusCode >= 200 && r.StatusCode < 300 {
			// The upstream parser deliberately tolerates HTML and unrecognized
			// lines; a nonempty file without any robots directive is not proof
			// that crawling is allowed (often it is a bot-block page).
			valid := recognizableRobots(r.Body)
			data, parseErr := robotstxt.FromBytes(r.Body)
			if parseErr != nil || !valid {
				st.robotsError = ErrRobots
			} else {
				st.robots = data.FindGroup(UserAgent)
				if st.robots.CrawlDelay > st.delay {
					st.delay = st.robots.CrawlDelay
				}
				st.next = time.Now().Add(st.delay)
			}
		} else {
			st.robotsError = ErrRobots
		}
	}
	if st.robotsError != nil {
		if errors.Is(st.robotsError, ErrAudit) || errors.Is(st.robotsError, ErrBudget) || errors.Is(st.robotsError, ErrCanceled) || errors.Is(st.robotsError, ErrPolicy) {
			return Response{}, nil, st.robotsError
		}
		return Response{}, nil, ErrRobots
	}
	if st.robots == nil || !st.robots.Test(u.RequestURI()) {
		return Response{}, nil, ErrRobots
	}
	return s.roundTrip(ctx, u, permission, st)
}

func recognizableRobots(body []byte) bool {
	hasAgent := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "user-agent":
			if strings.TrimSpace(value) == "" {
				return false
			}
			hasAgent = true
		case "allow", "disallow", "crawl-delay":
			if !hasAgent {
				return false
			}
		case "sitemap", "host":
		default:
			// Unknown extensions are not a reliable authorization policy.
			return false
		}
	}
	return true
}

func (s *Session) roundTrip(ctx context.Context, u *url.URL, permission allowedOrigin, st *originState) (Response, http.Header, error) {
	if err := waitUntil(ctx, st.next); err != nil {
		return Response{}, nil, err
	}
	if err := s.request(ctx); err != nil {
		return Response{}, nil, err
	}
	ctx = context.WithValue(ctx, dialKey{}, dialPermission{host: u.Hostname(), private: permission.private})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Response{}, nil, ErrPolicy
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept", "*/*")
	resp, err := s.transport.RoundTrip(req)
	st.next = time.Now().Add(st.delay)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, nil, ErrCanceled
		}
		if errors.Is(err, ErrPolicy) {
			return Response{}, nil, ErrPolicy
		}
		return Response{}, nil, ErrTransport
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode == 503 {
		if retry := retryTime(resp.Header.Get("Retry-After")); retry.After(st.next) {
			st.next = retry
		}
	}
	if resp.ContentLength > s.budget.MaxResponseBytes {
		return Response{}, nil, ErrBudget
	}
	wire, err := s.readBounded(ctx, resp.Body)
	if err != nil {
		return Response{}, nil, err
	}
	body := wire
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return Response{}, nil, ErrTransport
		}
		body, err = s.readBounded(ctx, reader)
		_ = reader.Close()
		if err != nil {
			return Response{}, nil, err
		}
	default:
		return Response{}, nil, ErrTransport
	}
	return Response{URL: u.String(), StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}, resp.Header, nil
}

func (s *Session) readBounded(ctx context.Context, r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	buf := make([]byte, 16<<10)
	for {
		// Read at most one byte beyond the per-response cap. The rejected
		// chunk is still charged, but never exposed as complete evidence.
		remaining := s.budget.MaxResponseBytes - int64(out.Len()) + 1
		if remaining < int64(len(buf)) {
			buf = buf[:remaining]
		}
		n, err := s.readChunk(ctx, r, buf)
		if n > 0 {
			if int64(out.Len()+n) > s.budget.MaxResponseBytes {
				return nil, ErrBudget
			}
			_, _ = out.Write(buf[:n])
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			if errors.Is(err, ErrBudget) || errors.Is(err, ErrAudit) || errors.Is(err, ErrCanceled) {
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ErrCanceled
			}
			return nil, ErrTransport
		}
		if ctx.Err() != nil {
			return nil, ErrCanceled
		}
	}
}

func retryTime(value string) time.Time {
	if n, err := strconv.ParseInt(value, 10, 32); err == nil && n >= 0 {
		return time.Now().Add(time.Duration(n) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil {
		return at
	}
	return time.Time{}
}
