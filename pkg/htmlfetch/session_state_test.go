package htmlfetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func sessionTestURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}

func sessionTestCookieValues(session *Session, raw string) []string {
	var values []string
	for _, cookie := range session.Cookies(sessionTestURL(raw)) {
		values = append(values, cookie.Name+"="+cookie.Value)
	}
	return values
}

func TestSessionStateRoundTripScopesAndMetadata(t *testing.T) {
	session := NewSession()
	origin := sessionTestURL("https://app.example.com/accounts/login?token=secret#fragment")
	hostCookie := &http.Cookie{Name: "host", Value: "host value", Quoted: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Raw: "original header", Unparsed: []string{"Priority=High"}}
	session.SetCookies(origin, []*http.Cookie{
		hostCookie,
		{Name: "domain", Value: "domain", Domain: ".example.com", Path: "/", Secure: true},
		{Name: "bad", Value: "suffix", Domain: "com", Path: "/"},
		{Name: "bad", Value: "other", Domain: "other.example", Path: "/"},
	})
	hostCookie.Unparsed[0] = "mutated input"
	session.SetCookies(sessionTestURL("https://other.test/"), []*http.Cookie{{Name: "other", Value: "other"}})
	state := session.Snapshot()
	if len(state.Cookies) != 3 {
		t.Fatalf("snapshot includes rejected cookies or misses a site: %#v", state)
	}
	host := state.Cookies[0]
	if host.SourceURL != "https://app.example.com/accounts/login" || host.Cookie.Path != "/accounts" || host.Cookie.Domain != "" || !host.Cookie.HttpOnly || !host.Cookie.Quoted || host.Cookie.SameSite != http.SameSiteStrictMode || host.Cookie.Unparsed[0] != "Priority=High" || host.Cookie.Raw != "original header" {
		t.Fatalf("metadata changed: %#v", host)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SessionState
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreSession(decoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded.Cookies[0].Cookie.Unparsed[0] = "mutated restore input"
	state.Cookies[0].Cookie.Unparsed[0] = "mutated snapshot"
	for label, candidate := range map[string]*Session{"original": session, "restored": restored} {
		if candidate.Snapshot().Cookies[0].Cookie.Unparsed[0] != "Priority=High" {
			t.Errorf("%s aliases external attributes", label)
		}
		for _, test := range []struct {
			url  string
			want []string
		}{
			{"https://app.example.com/accounts/me", []string{"host=host value", "domain=domain"}},
			{"https://sub.app.example.com/accounts/me", []string{"domain=domain"}},
			{"https://app.example.com/accounts-other", []string{"domain=domain"}},
			{"http://app.example.com/accounts/me", []string{"host=host value"}},
			{"https://example.com/", []string{"domain=domain"}},
			{"https://other.test/", []string{"other=other"}},
			{"https://unrelated.test/", nil},
		} {
			if got := sessionTestCookieValues(candidate, test.url); !reflect.DeepEqual(got, test.want) {
				t.Errorf("%s %s: got %v want %v", label, test.url, got, test.want)
			}
		}
	}
}

func TestSessionStateMaxAgeDoesNotRenew(t *testing.T) {
	session := NewSession()
	before := time.Now()
	session.SetCookies(sessionTestURL("https://example.com/"), []*http.Cookie{{Name: "sid", Value: "secret", MaxAge: 3600, Expires: before.Add(-time.Hour)}})
	state := session.Snapshot()
	cookie := state.Cookies[0].Cookie
	if cookie.MaxAge != 0 || cookie.Expires.Before(before.Add(time.Hour)) || cookie.Expires.After(time.Now().Add(time.Hour)) {
		t.Fatalf("MaxAge did not become a fixed deadline: %#v", cookie)
	}
	restored, err := RestoreSession(state)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Snapshot().Cookies[0].Cookie.Expires.Equal(cookie.Expires) {
		t.Fatal("restore renewed MaxAge")
	}
	state.Cookies[0].Cookie.Expires = before.Add(-time.Second)
	expired, err := RestoreSession(state)
	if err != nil || len(expired.Cookies(sessionTestURL("https://example.com/"))) != 0 || len(expired.Snapshot().Cookies) != 0 {
		t.Fatalf("expired cookie resurrected: %v", err)
	}
}

func TestSessionStateReplacementDeletionAndOrder(t *testing.T) {
	session := NewSession()
	origin := sessionTestURL("https://app.example.com/a/login")
	session.SetCookies(origin, []*http.Cookie{{Name: "first", Value: "old"}, {Name: "second", Value: "2"}})
	for i := 0; i < 100; i++ {
		session.SetCookies(origin, []*http.Cookie{{Name: "first", Value: "new", Domain: "app.example.com"}})
	}
	state := session.Snapshot()
	if len(state.Cookies) != 2 || state.Cookies[0].Cookie.Name != "first" {
		t.Fatal("replacement appended history or reordered cookies")
	}
	restored, err := RestoreSession(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionTestCookieValues(restored, origin.String()); !reflect.DeepEqual(got, []string{"first=new", "second=2"}) {
		t.Fatalf("restore changed creation ordering: %v", got)
	}
	if got := sessionTestCookieValues(restored, "https://sub.app.example.com/a/"); !reflect.DeepEqual(got, []string{"first=new"}) {
		t.Fatalf("replacement did not update host-only semantics: %v", got)
	}
	// The host-only/domain flag is not part of the domain/path/name identity.
	restored.SetCookies(origin, []*http.Cookie{{Name: "first", MaxAge: -1}, {Name: "second", Expires: time.Now().Add(-time.Hour)}})
	if len(restored.Snapshot().Cookies) != 0 || len(restored.Cookies(origin)) != 0 {
		t.Fatal("deletion left snapshot or request cookies")
	}
}

func TestSessionStateKeepsDistinctDomainAndPathScopes(t *testing.T) {
	session := NewSession()
	origin := sessionTestURL("https://app.example.com/private/login")
	session.SetCookies(origin, []*http.Cookie{
		{Name: "sid", Value: "root", Path: "/"},
		{Name: "sid", Value: "private", Path: "/private"},
		{Name: "sid", Value: "domain", Domain: ".example.com", Path: "/"},
	})
	// A foreign-domain deletion must not delete any accepted cookie.
	session.SetCookies(origin, []*http.Cookie{{Name: "sid", Domain: "com", Path: "/", MaxAge: -1}})
	state := session.Snapshot()
	if len(state.Cookies) != 3 {
		t.Fatalf("distinct scopes were collapsed: %#v", state)
	}
	restored, err := RestoreSession(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionTestCookieValues(restored, origin.String()); !reflect.DeepEqual(got, []string{"sid=private", "sid=root", "sid=domain"}) {
		t.Fatalf("path ordering or identical names changed: %v", got)
	}
	if got := sessionTestCookieValues(restored, "https://sibling.example.com/private/"); !reflect.DeepEqual(got, []string{"sid=domain"}) {
		t.Fatalf("host-only cookies leaked to sibling: %v", got)
	}
	restored.SetCookies(origin, []*http.Cookie{{Name: "sid", Path: "/private", MaxAge: -1}})
	if len(restored.Snapshot().Cookies) != 2 {
		t.Fatal("path-specific deletion affected other scopes")
	}
}

func TestSessionStateRecordsAutomaticResponseCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/login" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "secret", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
			http.Redirect(w, r, "/done", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "done", Value: "yes", Path: "/"})
	}))
	defer server.Close()
	session := NewSession()
	_, err := NewClient(Config{Network: egress.Config{PrivateOrigins: []string{server.URL}}}).Do(WithSession(context.Background(), session), Request{URL: server.URL + "/account/login"})
	if err != nil {
		t.Fatal(err)
	}
	state := session.Snapshot()
	if len(state.Cookies) != 2 || state.Cookies[0].SourceURL != server.URL+"/account/login" || state.Cookies[0].Cookie.Path != "/account" || !state.Cookies[0].Cookie.HttpOnly || state.Cookies[0].Cookie.SameSite != http.SameSiteLaxMode || state.Cookies[0].Cookie.Expires.IsZero() {
		t.Fatalf("response/redirect metadata missing: %#v", state)
	}
	restored, err := RestoreSession(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionTestCookieValues(restored, server.URL+"/account/me"); !reflect.DeepEqual(got, []string{"sid=secret", "done=yes"}) {
		t.Fatalf("automatically received cookies not restored: %v", got)
	}
}

func TestSessionStateCanonicalScope(t *testing.T) {
	for _, test := range []struct{ origin, alternate, domain string }{
		{"https://EXAMPLE.com:443/a", "https://example.com./a", "example.com"},
		{"https://bücher.example/a", "https://xn--bcher-kva.example/a", "xn--bcher-kva.example"},
		{"https://127.0.0.1:443/a", "https://127.0.0.1/a", "127.0.0.1"},
		{"https://[::1]:443/a", "https://[::1]:8443/a", "::1"},
		{"https://co.uk/a", "https://co.uk/a", "co.uk"},
	} {
		t.Run(test.origin, func(t *testing.T) {
			session := NewSession()
			session.SetCookies(sessionTestURL(test.origin), []*http.Cookie{{Name: "sid", Value: "first"}})
			session.SetCookies(sessionTestURL(test.alternate), []*http.Cookie{{Name: "sid", Value: "second", Domain: test.domain}})
			state := session.Snapshot()
			if len(state.Cookies) != 1 || state.Cookies[0].Cookie.Value != "second" {
				t.Fatalf("canonical replacement duplicated scope: %#v", state)
			}
			restored, err := RestoreSession(state)
			if err != nil {
				t.Fatal(err)
			}
			if got := sessionTestCookieValues(restored, test.origin); !reflect.DeepEqual(got, []string{"sid=second"}) {
				t.Fatalf("canonical scope not restored: %v", got)
			}
		})
	}
}

func TestSessionStateRejectsInvalidRestore(t *testing.T) {
	valid := func() SessionState {
		return SessionState{Version: SessionStateVersion, Cookies: []SessionCookie{{SourceURL: "https://example.com/login", Cookie: http.Cookie{Name: "sid", Value: "secret", Path: "/"}}}}
	}
	for name, mutate := range map[string]func(*SessionState){
		"version":     func(s *SessionState) { s.Version++ },
		"count":       func(s *SessionState) { s.Cookies = make([]SessionCookie, MaxSessionStateCookies+1) },
		"size":        func(s *SessionState) { s.Cookies[0].Cookie.Value = strings.Repeat("x", MaxSessionStateBytes) },
		"attributes":  func(s *SessionState) { s.Cookies[0].Cookie.Unparsed = make([]string, 65) },
		"partitioned": func(s *SessionState) { s.Cookies[0].Cookie.Partitioned = true },
		"max-age":     func(s *SessionState) { s.Cookies[0].Cookie.MaxAge = 600 },
		"deleted":     func(s *SessionState) { s.Cookies[0].Cookie.MaxAge = -1 },
		"path":        func(s *SessionState) { s.Cookies[0].Cookie.Path = "relative" },
		"scheme":      func(s *SessionState) { s.Cookies[0].SourceURL = "ftp://example.com/" },
		"host":        func(s *SessionState) { s.Cookies[0].SourceURL = "https:///path" },
		"userinfo":    func(s *SessionState) { s.Cookies[0].SourceURL = "https://user:pass@example.com/" },
		"suffix":      func(s *SessionState) { s.Cookies[0].Cookie.Domain = "com" },
		"foreign":     func(s *SessionState) { s.Cookies[0].Cookie.Domain = "other.test" },
		"duplicate":   func(s *SessionState) { s.Cookies = append(s.Cookies, s.Cookies[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			state := valid()
			mutate(&state)
			if session, err := RestoreSession(state); err == nil || session != nil {
				t.Fatalf("invalid snapshot accepted: session=%v err=%v", session, err)
			}
		})
	}
}

func TestSessionStateConcurrentSnapshotAndMutation(t *testing.T) {
	session := NewSession()
	origin := sessionTestURL("https://example.com/a/login")
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				name := fmt.Sprintf("cookie%d", worker)
				session.SetCookies(origin, []*http.Cookie{{Name: name, Value: fmt.Sprint(i), Unparsed: []string{"Priority=High"}}})
				_ = session.Cookies(origin)
				state := session.Snapshot()
				if len(state.Cookies) > 8 {
					t.Error("snapshot accumulated replacement history")
				}
				if _, err := RestoreSession(state); err != nil {
					t.Errorf("concurrent snapshot is inconsistent: %v", err)
				}
				if i%3 == 0 {
					session.SetCookies(origin, []*http.Cookie{{Name: name, MaxAge: -1}})
				}
			}
		}(worker)
	}
	wg.Wait()
}
