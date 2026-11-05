package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// CopyCookiesToSession copies only cookies applicable to rawURL from a live page.
// The caller owns the page/account context. This does not isolate browser accounts
// or copy LocalStorage. Partitioned cookies are rejected, never widened silently.
func CopyCookiesToSession(page *rod.Page, session *htmlfetch.Session, rawURL string) error {
	if page == nil || session == nil {
		return fmt.Errorf("cookie transfer: page and session are required")
	}
	origin, err := url.Parse(rawURL)
	if err != nil || origin.Hostname() == "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return fmt.Errorf("cookie transfer: valid HTTP origin required")
	}
	cookies, err := page.Cookies([]string{rawURL})
	if err != nil {
		return fmt.Errorf("read browser cookies: %w", err)
	}
	converted, err := httpCookies(cookies)
	if err != nil {
		return err
	}
	session.SetCookies(origin, converted)
	return nil
}

func httpCookies(cookies []*proto.NetworkCookie) ([]*http.Cookie, error) {
	out := make([]*http.Cookie, 0, len(cookies))
	for _, c := range cookies {
		if c == nil {
			continue
		}
		if c.PartitionKey != nil || c.PartitionKeyOpaque {
			return nil, fmt.Errorf("cookie transfer: partitioned cookies are unsupported")
		}
		domain := c.Domain
		// Chromium marks domain cookies with a leading dot. Preserve host-only
		// cookies by leaving Domain empty; otherwise subdomains would gain access.
		if !strings.HasPrefix(domain, ".") {
			domain = ""
		}
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if !c.Session {
			cookie.Expires = time.Unix(int64(c.Expires), 0)
		}
		switch c.SameSite {
		case proto.NetworkCookieSameSiteStrict:
			cookie.SameSite = http.SameSiteStrictMode
		case proto.NetworkCookieSameSiteLax:
			cookie.SameSite = http.SameSiteLaxMode
		case proto.NetworkCookieSameSiteNone:
			cookie.SameSite = http.SameSiteNoneMode
		}
		out = append(out, cookie)
	}
	return out, nil
}
