// Package discovery discovers bounded crawl targets without submitting tasks.
package discovery

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// CanonicalURL resolves HTTP links, removes fragments and normalizes host/default
// port. Query order/values and path escaping remain intact for signed URLs.
func CanonicalURL(base, reference string) (string, error) {
	b, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	r, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return "", err
	}
	u := b.ResolveReference(r)
	u.Scheme = strings.ToLower(u.Scheme)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("expected HTTP URL without userinfo")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	u.Fragment = ""
	u.RawFragment = ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// SameOrigin is strict: scheme, hostname and effective port all matter.
func SameOrigin(a, b string) bool {
	x, err := CanonicalURL(a, "")
	if err != nil {
		return false
	}
	y, err := CanonicalURL(b, "")
	if err != nil {
		return false
	}
	u, _ := url.Parse(x)
	v, _ := url.Parse(y)
	return u.Scheme == v.Scheme && u.Host == v.Host
}

type LinkOptions struct {
	Selector      string // default a[href]; can select link[rel=next]
	Attribute     string // default href
	AllowExternal bool
	MaxLinks      int               // default 10000
	Allow         func(string) bool // optional additional policy, e.g. robots rules
}
type Links struct {
	URLs      []string
	Truncated bool
}

func ExtractLinks(page *htmlfetch.Page, opts LinkOptions) (Links, error) {
	var out Links
	if page == nil || page.URL == nil || page.Document == nil {
		return out, fmt.Errorf("page URL and document required")
	}
	if opts.Selector == "" {
		opts.Selector = "a[href]"
	}
	if opts.Attribute == "" {
		opts.Attribute = "href"
	}
	if opts.MaxLinks <= 0 {
		opts.MaxLinks = 10000
	}
	selector, err := cascadia.Compile(opts.Selector)
	if err != nil {
		return out, err
	}
	origin := page.URL.String()
	base := origin
	if href, ok := page.Selection().Find("base[href]").First().Attr("href"); ok {
		if resolved, e := CanonicalURL(origin, href); e == nil {
			base = resolved
		}
	}
	seen := map[string]bool{}
	page.Selection().FindMatcher(selector).EachWithBreak(func(_ int, node *goquery.Selection) bool {
		ref, ok := node.Attr(opts.Attribute)
		if !ok || strings.TrimSpace(ref) == "" || strings.HasPrefix(strings.TrimSpace(ref), "#") {
			return true
		}
		u, e := CanonicalURL(base, ref)
		if e != nil || seen[u] || (!opts.AllowExternal && !SameOrigin(origin, u)) || (opts.Allow != nil && !opts.Allow(u)) {
			return true
		}
		if len(out.URLs) >= opts.MaxLinks {
			out.Truncated = true
			return false
		}
		seen[u] = true
		out.URLs = append(out.URLs, u)
		return true
	})
	return out, nil
}
