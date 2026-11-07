package discovery

import (
	"reflect"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestLinksCanonicalScopeAndBase(t *testing.T) {
	page, _ := htmlfetch.NewRenderedPage("https://EXAMPLE.com:443/start", `<base href="/books/"><a href="one#part">one</a><a href="one">duplicate</a><a href="//evil.com/x">external</a><a href="javascript:alert(1)">bad</a><a href="https://example.com:444/x">port</a><a href="#top">fragment</a><a href="two?b=2&a=1">two</a>`)
	links, err := ExtractLinks(page, LinkOptions{})
	if err != nil || !reflect.DeepEqual(links.URLs, []string{"https://example.com/books/one", "https://example.com/books/two?b=2&a=1"}) {
		t.Fatalf("links: %+v %v", links, err)
	}
	links, err = ExtractLinks(page, LinkOptions{MaxLinks: 1})
	if err != nil || !links.Truncated {
		t.Fatalf("limit: %+v %v", links, err)
	}
	if SameOrigin("http://example.com", "https://example.com") || SameOrigin("https://example.com", "https://www.example.com") {
		t.Fatal("origin scope widened")
	}
	if _, err := ExtractLinks(page, LinkOptions{Selector: "["}); err == nil {
		t.Fatal("bad selector accepted")
	}
	page, _ = htmlfetch.NewRenderedPage("https://example.com", `<base href="https://evil.com/"><a href="one">x</a>`)
	links, _ = ExtractLinks(page, LinkOptions{})
	if len(links.URLs) != 0 {
		t.Fatal("base escaped origin")
	}
}

func TestXMLFeedsAndLimits(t *testing.T) {
	for _, tc := range []struct {
		xml   string
		index bool
	}{
		{`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>/one</loc></url></urlset>`, false},
		{`<sitemapindex><sitemap><loc>/one</loc></sitemap></sitemapindex>`, true},
		{`<rss><channel><item><link>/one</link></item></channel></rss>`, false},
		{`<feed xmlns="http://www.w3.org/2005/Atom"><entry><link rel="self" href="/skip"/><link href="/one"/></entry></feed>`, false},
	} {
		out, err := ParseXML([]byte(tc.xml), "https://example.com/feed", 10)
		if err != nil || len(out.Targets) != 1 || out.Targets[0].URL != "https://example.com/one" || out.Targets[0].Sitemap != tc.index {
			t.Fatalf("feed: %+v %v", out, err)
		}
	}
	if _, err := ParseXML([]byte(`<urlset/><urlset/>`), "https://example.com", 1); err == nil {
		t.Fatal("multiple roots accepted")
	}
	out, err := ParseXML([]byte(`<urlset><url><loc>/a</loc></url><url><loc>/b</loc></url></urlset>`), "https://example.com", 1)
	if err != nil || !out.Truncated || len(out.Targets) != 1 {
		t.Fatalf("limit: %+v %v", out, err)
	}
}

func TestRobotsAgentScopeAndRules(t *testing.T) {
	r, err := ParseRobots("https://example.com", "PapaBot", []byte("User-agent: *\nDisallow: /private\nAllow: /private/public\nCrawl-delay: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Allowed("https://example.com/private/a") || !r.Allowed("https://example.com/private/public") || r.Allowed("https://evil.com/") || r.CrawlDelay().Seconds() != 2 {
		t.Fatal("robots policy mismatch")
	}
	empty, err := ParseRobots("https://example.com", "PapaBot", nil)
	if err != nil || !empty.Allowed("https://example.com/") {
		t.Fatalf("empty robots: %v", err)
	}
}

func TestPaginationStopReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		body   []byte
		keys   []string
		next   bool
		reason string
	}{
		{"cycle", "https://example.com/1#frag", nil, nil, true, "url-cycle"},
		{"content", "https://example.com/2", []byte("page"), nil, true, "repeated-content"},
		{"items", "https://example.com/2", nil, []string{"one"}, true, "no-new-items"},
		{"end", "https://example.com/2", nil, []string{"two"}, false, "no-next-page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := NewPaginationGuard(3, 10)
			first, err := g.Observe("https://example.com/1", []byte("page"), []string{"one"}, true)
			if err != nil || !first.Continue {
				t.Fatal(first, err)
			}
			d, err := g.Observe(tc.url, tc.body, tc.keys, tc.next)
			if err != nil || d.Continue || d.Reason != tc.reason {
				t.Fatal(d, err)
			}
			again, _ := g.Observe("https://example.com/3", nil, nil, true)
			if again.Reason != tc.reason {
				t.Fatal("stop not sticky")
			}
		})
	}
	g, _ := NewPaginationGuard(1, 10)
	d, _ := g.Observe("https://example.com/", nil, nil, true)
	if d.Reason != "page-limit" {
		t.Fatal(d)
	}
	g, _ = NewPaginationGuard(3, 1)
	d, _ = g.Observe("https://example.com/", nil, []string{"a", "b"}, true)
	if d.Reason != "item-limit" || d.NewItems != 1 {
		t.Fatal(d)
	}
}
