package discovery

import (
	"fmt"
	"net/url"
	"time"

	"github.com/temoto/robotstxt"
)

type Robots struct {
	origin string
	group  *robotstxt.Group
}

// ParseRobots parses already-fetched rules for an origin and crawler agent.
// Caller owns HTTP status policy, caching, redirects and crawl-delay scheduling.
func ParseRobots(origin, agent string, body []byte) (*Robots, error) {
	u, err := CanonicalURL(origin, "")
	if err != nil {
		return nil, err
	}
	if agent == "" {
		return nil, fmt.Errorf("robots agent required")
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("robots exceeds 1 MiB")
	}
	data, err := robotstxt.FromBytes(body)
	if err != nil {
		return nil, err
	}
	return &Robots{origin: u, group: data.FindGroup(agent)}, nil
}

func (r *Robots) Allowed(rawURL string) bool {
	if r == nil || !SameOrigin(r.origin, rawURL) {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return r.group.Test(path)
}
func (r *Robots) CrawlDelay() time.Duration {
	if r == nil {
		return 0
	}
	return r.group.CrawlDelay
}
