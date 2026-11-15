package browser

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func cookieForHost(entry htmlfetch.SessionCookie, host string) bool {
	c := entry.Cookie
	if c.Domain != "" {
		d := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		return host == d || strings.HasSuffix(host, "."+d)
	}
	u, err := url.Parse(entry.SourceURL)
	return err == nil && strings.EqualFold(u.Hostname(), host)
}

// PageStateFromSession copies scoped cookies with their original attributes.
// It does not infer LocalStorage or send requests. The result contains secrets.
func PageStateFromSession(session *htmlfetch.Session, rawURL string) (*PageState, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || session == nil {
		return nil, errors.New("session bridge requires a session and HTTP URL")
	}
	snapshot := session.Snapshot()
	if _, err := htmlfetch.RestoreSession(snapshot); err != nil {
		return nil, err
	}
	state := &PageState{Version: 1, Origin: u.Scheme + "://" + u.Host, LocalStorage: map[string]string{}}
	for _, entry := range snapshot.Cookies {
		if !cookieForHost(entry, strings.ToLower(u.Hostname())) {
			continue
		}
		c := entry.Cookie
		if c.Partitioned {
			return nil, errors.New("session bridge cannot transfer partitioned cookies")
		}
		domain := u.Hostname()
		if c.Domain != "" && net.ParseIP(strings.TrimPrefix(c.Domain, ".")) == nil {
			domain = "." + strings.TrimPrefix(c.Domain, ".")
		}
		cookie := &proto.NetworkCookie{Name: c.Name, Value: c.Value, Domain: domain, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HttpOnly, Session: c.Expires.IsZero()}
		if !cookie.Session {
			cookie.Expires = proto.TimeSinceEpoch(float64(c.Expires.Unix()))
		}
		switch c.SameSite {
		case http.SameSiteStrictMode:
			cookie.SameSite = proto.NetworkCookieSameSiteStrict
		case http.SameSiteLaxMode:
			cookie.SameSite = proto.NetworkCookieSameSiteLax
		case http.SameSiteNoneMode:
			cookie.SameSite = proto.NetworkCookieSameSiteNone
		}
		state.Cookies = append(state.Cookies, cookie)
	}
	return state, ValidatePageState(state)
}

// SessionWithPageState replaces the origin's cookie scopes, including deletions,
// while retaining unrelated origins. Neither input is modified.
func SessionWithPageState(session *htmlfetch.Session, state *PageState) (*htmlfetch.Session, error) {
	if session == nil {
		return nil, errors.New("session bridge requires a session")
	}
	if err := ValidatePageState(state); err != nil {
		return nil, err
	}
	u, _ := url.Parse(state.Origin)
	snapshot := session.Snapshot()
	kept := snapshot.Cookies[:0]
	for _, entry := range snapshot.Cookies {
		if !cookieForHost(entry, strings.ToLower(u.Hostname())) {
			kept = append(kept, entry)
		}
	}
	snapshot.Cookies = kept
	result, err := htmlfetch.RestoreSession(snapshot)
	if err != nil {
		return nil, err
	}
	cookies, err := httpCookies(state.Cookies)
	if err != nil {
		return nil, err
	}
	result.SetCookies(u, cookies)
	return result, nil
}
