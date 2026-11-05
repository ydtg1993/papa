package discovery

import (
	"crypto/sha256"
	"fmt"
	"sync"
)

type PaginationDecision struct {
	Continue bool
	Reason   string
	NewItems int
	Pages    int
}

// PaginationGuard bounds one sequential crawl traversal; not a task queue.
// Keys must be stable business IDs, not array indexes or random request tokens.
type PaginationGuard struct {
	mu                 sync.Mutex
	maxPages, maxItems int
	urls               map[string]bool
	bodies             map[[32]byte]bool
	items              map[string]bool
	stopped            string
}

func NewPaginationGuard(maxPages, maxItems int) (*PaginationGuard, error) {
	if maxPages <= 0 || maxItems <= 0 {
		return nil, fmt.Errorf("positive page and item limits required")
	}
	return &PaginationGuard{maxPages: maxPages, maxItems: maxItems, urls: map[string]bool{}, bodies: map[[32]byte]bool{}, items: map[string]bool{}}, nil
}

// Observe checks a successfully parsed page. nil body disables content hashing;
// an empty non-nil body is a real empty response. nil itemKeys disables item
// progress checking; an empty non-nil slice explicitly reports no items.
// Continue grants permission for a next page, not acceptance of the current data.
func (g *PaginationGuard) Observe(rawURL string, body []byte, itemKeys []string, hasNext bool) (PaginationDecision, error) {
	u, err := CanonicalURL(rawURL, "")
	if err != nil {
		return PaginationDecision{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	d := PaginationDecision{Pages: len(g.urls)}
	stop := func(reason string) PaginationDecision { g.stopped = reason; d.Reason = reason; return d }
	if g.stopped != "" {
		return stop(g.stopped), nil
	}
	if g.urls[u] {
		return stop("url-cycle"), nil
	}
	if body != nil {
		hash := sha256.Sum256(body)
		if g.bodies[hash] {
			return stop("repeated-content"), nil
		}
		g.bodies[hash] = true
	}
	g.urls[u] = true
	d.Pages = len(g.urls)
	for _, key := range itemKeys {
		if key == "" || g.items[key] {
			continue
		}
		if len(g.items) >= g.maxItems {
			return stop("item-limit"), nil
		}
		g.items[key] = true
		d.NewItems++
	}
	if itemKeys != nil && d.NewItems == 0 {
		return stop("no-new-items"), nil
	}
	if !hasNext {
		return stop("no-next-page"), nil
	}
	if d.Pages >= g.maxPages {
		return stop("page-limit"), nil
	}
	if len(g.items) >= g.maxItems {
		return stop("item-limit"), nil
	}
	d.Continue = true
	return d, nil
}
