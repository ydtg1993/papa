package htmlfetch

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"

	"golang.org/x/net/publicsuffix"
)

type sessionKey struct{}

// Session holds an independent, concurrent-safe in-memory CookieJar.
// Use one session per account/site. It does not persist credentials or pin proxies.
type Session struct{ jar *cookiejar.Jar }

func NewSession() *Session {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &Session{jar: jar}
}

// WithSession enables automatic cookies only for requests using this context.
// Passing nil disables an inherited session. Explicit Cookie headers are additional
// cookies, following net/http semantics; do not mix them with a different account.
func WithSession(ctx context.Context, session *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, session)
}

// SetCookies imports cookies for a known origin, applying standard domain/path rules.
func (s *Session) SetCookies(origin *url.URL, cookies []*http.Cookie) {
	s.jar.SetCookies(origin, cookies)
}

// Cookies returns cookies applicable to origin, not a credential export of all sites.
func (s *Session) Cookies(origin *url.URL) []*http.Cookie { return s.jar.Cookies(origin) }
