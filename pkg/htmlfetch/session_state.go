package htmlfetch

import (
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

const SessionStateVersion = 1

// Restore limits apply to the decoded state, in addition to any limits the
// caller must apply before decoding an untrusted serialized credential file.
const (
	MaxSessionStateCookies = 4096
	MaxSessionStateBytes   = 4 << 20
)

// SessionState contains secrets. Encrypt it before persistence and never log it.
// Cookies are ordered by creation, preserving request ordering after restore.
// Session cookies (without Expires) are intentionally included for resumption.
type SessionState struct {
	Version int             `json:"version"`
	Cookies []SessionCookie `json:"cookies"`
}

// SessionCookie retains the received cookie's attributes and source URL.
// Path is resolved at receipt. MaxAge is normalized to zero and Expires holds
// its absolute deadline, so restoring a snapshot cannot renew the lifetime.
// Empty Domain continues to mean host-only; it is never widened to a domain.
// Raw and RawExpires are informational original header data, not replayed.
type SessionCookie struct {
	SourceURL string      `json:"source_url"`
	Cookie    http.Cookie `json:"cookie"`
}

type sessionCookieKey struct{ domain, path, name string }
type sessionCookieEntry struct {
	state SessionCookie
	order uint64
}

// All accesses, including those made automatically by http.Client, go through
// this wrapper. The standard jar remains responsible for request selection.
type sessionJar struct {
	mu      sync.Mutex
	jar     *cookiejar.Jar
	entries map[sessionCookieKey]sessionCookieEntry
	next    uint64
}

var _ http.CookieJar = (*sessionJar)(nil)

func newSessionJar() *sessionJar {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &sessionJar{jar: jar, entries: make(map[sessionCookieKey]sessionCookieEntry)}
}

func (j *sessionJar) Cookies(origin *url.URL) []*http.Cookie {
	if origin == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.jar.Cookies(origin)
}

func (j *sessionJar) SetCookies(origin *url.URL, cookies []*http.Cookie) {
	if origin == nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	j.removeExpired(now)
	for _, received := range cookies {
		if received == nil {
			continue
		}
		cookie := copySessionCookie(*received)
		if cookie.Path == "" || cookie.Path[0] != '/' {
			cookie.Path = sessionDefaultPath(origin.Path)
		}
		key, ok := acceptedSessionCookieKey(origin, &cookie)
		if !ok {
			continue
		}
		if cookie.MaxAge > 0 {
			cookie.Expires = now.Add(time.Duration(cookie.MaxAge) * time.Second)
			cookie.MaxAge = 0
		}
		j.jar.SetCookies(origin, []*http.Cookie{&cookie})
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !cookie.Expires.After(now)) {
			delete(j.entries, key)
			continue
		}
		old, exists := j.entries[key]
		if !exists {
			old.order = j.next
			j.next++
		}
		// Query strings, fragments and URL userinfo are not needed for cookie
		// scope and may contain unrelated credentials.
		source := &url.URL{Scheme: origin.Scheme, Host: origin.Host, Path: origin.Path, RawPath: origin.RawPath}
		old.state = SessionCookie{SourceURL: source.String(), Cookie: cookie}
		j.entries[key] = old
	}
}

// Snapshot returns an independent copy of all unexpired received cookies,
// including cookies which do not apply to the most recently requested URL.
func (s *Session) Snapshot() SessionState {
	s.jar.mu.Lock()
	defer s.jar.mu.Unlock()
	s.jar.removeExpired(time.Now())
	entries := make([]sessionCookieEntry, 0, len(s.jar.entries))
	for _, entry := range s.jar.entries {
		entry.state.Cookie = copySessionCookie(entry.state.Cookie)
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, k int) bool { return entries[i].order < entries[k].order })
	state := SessionState{Version: SessionStateVersion, Cookies: make([]SessionCookie, 0, len(entries))}
	for _, entry := range entries {
		state.Cookies = append(state.Cookies, entry.state)
	}
	return state
}

// RestoreSession validates the complete snapshot before returning a session.
// Expired cookies are discarded. Partitioned cookies are rejected because the
// standard HTTP cookie jar cannot represent their partition keys. SameSite is
// retained as metadata; Go's HTTP jar does not enforce browser SameSite rules.
func RestoreSession(state SessionState) (*Session, error) {
	if state.Version != SessionStateVersion {
		return nil, fmt.Errorf("htmlfetch: unsupported session state version %d", state.Version)
	}
	if len(state.Cookies) > MaxSessionStateCookies {
		return nil, fmt.Errorf("htmlfetch: session state exceeds cookie count limit")
	}
	remaining := MaxSessionStateBytes
	seen := make(map[sessionCookieKey]struct{}, len(state.Cookies))
	session := NewSession()
	for i, entry := range state.Cookies {
		cookie := entry.Cookie
		if len(cookie.Unparsed) > 64 {
			return nil, fmt.Errorf("htmlfetch: session cookie %d has too many attributes", i)
		}
		fields := []string{entry.SourceURL, cookie.Name, cookie.Value, cookie.Path, cookie.Domain, cookie.Raw, cookie.RawExpires}
		fields = append(fields, cookie.Unparsed...)
		for _, field := range fields {
			if len(field) > remaining {
				return nil, fmt.Errorf("htmlfetch: session state exceeds size limit")
			}
			remaining -= len(field)
		}
		if cookie.Partitioned {
			return nil, fmt.Errorf("htmlfetch: session cookie %d is partitioned and cannot be restored", i)
		}
		if cookie.MaxAge != 0 {
			return nil, fmt.Errorf("htmlfetch: session cookie %d requires an absolute expiry", i)
		}
		if cookie.Path == "" || cookie.Path[0] != '/' {
			return nil, fmt.Errorf("htmlfetch: session cookie %d has an unresolved path", i)
		}
		origin, err := url.Parse(entry.SourceURL)
		if err != nil || origin.Host == "" || origin.User != nil || origin.Opaque != "" || (origin.Scheme != "http" && origin.Scheme != "https") {
			return nil, fmt.Errorf("htmlfetch: session cookie %d has an invalid source URL", i)
		}
		key, ok := acceptedSessionCookieKey(origin, &cookie)
		if !ok {
			return nil, fmt.Errorf("htmlfetch: session cookie %d has an invalid domain", i)
		}
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("htmlfetch: session cookie %d duplicates a cookie scope", i)
		}
		seen[key] = struct{}{}
		session.SetCookies(origin, []*http.Cookie{&cookie})
	}
	return session, nil
}

func copySessionCookie(cookie http.Cookie) http.Cookie {
	cookie.Unparsed = append([]string(nil), cookie.Unparsed...)
	return cookie
}

func (j *sessionJar) removeExpired(now time.Time) {
	for key, entry := range j.entries {
		if expiry := entry.state.Cookie.Expires; expiry.IsZero() || expiry.After(now) {
			continue
		}
		origin, _ := url.Parse(entry.state.SourceURL)
		cookie := entry.state.Cookie
		cookie.MaxAge = -1
		j.jar.SetCookies(origin, []*http.Cookie{&cookie})
		delete(j.entries, key)
	}
}

func sessionDefaultPath(path string) string {
	if path == "" || path[0] != '/' {
		return "/"
	}
	if last := strings.LastIndex(path, "/"); last > 0 {
		return path[:last]
	}
	return "/"
}

// Probe only the origin/domain acceptance using a separate standard jar. This
// avoids duplicating public-suffix, IP and malformed-domain acceptance rules.
// No request cookies are flattened or used to infer scope.
func acceptedSessionCookieKey(origin *url.URL, cookie *http.Cookie) (sessionCookieKey, bool) {
	probe, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	probeCookie := &http.Cookie{Name: "papa_scope_probe", Value: "1", Domain: cookie.Domain, Path: "/"}
	probe.SetCookies(origin, []*http.Cookie{probeCookie})
	probeURL := *origin
	probeURL.Path = "/"
	if len(probe.Cookies(&probeURL)) != 1 {
		return sessionCookieKey{}, false
	}
	domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
	if cookie.Domain == "" {
		host := origin.Host
		// Match net/http/cookiejar's port handling, including bracketed IPv6.
		colons := strings.Count(host, ":")
		if colons == 1 || (colons > 1 && strings.HasPrefix(host, "[") && strings.Contains(host, "]:")) {
			var err error
			host, _, err = net.SplitHostPort(host)
			if err != nil {
				return sessionCookieKey{}, false
			}
		}
		host = strings.TrimSuffix(host, ".")
		var err error
		domain, err = idna.ToASCII(host)
		if err != nil {
			return sessionCookieKey{}, false
		}
		domain = strings.ToLower(domain)
	}
	return sessionCookieKey{domain: domain, path: cookie.Path, name: cookie.Name}, true
}
