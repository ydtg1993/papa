package middleware

import (
	"container/list"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// ProxyTransportCacheCapacity bounds retained proxy transports per downloader family.
const ProxyTransportCacheCapacity = 64

// ProxyTransportCache is a zero-value-ready LRU shared by a downloader and With
// copies. Eviction retires connections without interrupting active responses.
// Callers must close response bodies, as required by net/http.
type ProxyTransportCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
}
type proxyTransportEntry struct {
	key     string
	base    http.RoundTripper
	retired atomic.Bool
}

// Get reuses a transport or creates it under the cache lock. create must not do
// network I/O or reenter this cache. Invalid/denied transports count toward the
// same capacity. Closing evicted connections happens outside the cache lock.
func (c *ProxyTransportCache) Get(key string, create func() http.RoundTripper) http.RoundTripper {
	c.mu.Lock()
	if e := c.entries[key]; e != nil {
		c.lru.MoveToFront(e)
		tr := e.Value.(*proxyTransportEntry)
		c.mu.Unlock()
		return tr
	}
	// Unlock on a factory panic without changing cache state.
	var base http.RoundTripper
	func() {
		defer func() {
			if v := recover(); v != nil {
				c.mu.Unlock()
				panic(v)
			}
		}()
		base = create()
	}()
	tr := &proxyTransportEntry{key: key, base: base}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.entries[key] = c.lru.PushFront(tr)
	var retired *proxyTransportEntry
	if c.lru.Len() > ProxyTransportCacheCapacity {
		e := c.lru.Back()
		retired = e.Value.(*proxyTransportEntry)
		retired.retired.Store(true)
		delete(c.entries, retired.key)
		c.lru.Remove(e)
	}
	c.mu.Unlock()
	if retired != nil {
		retired.CloseIdleConnections()
	}
	return tr
}

func (t *proxyTransportEntry) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
func (t *proxyTransportEntry) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if resp != nil && resp.Body != nil {
		resp.Body = &retiringBody{ReadCloser: resp.Body, transport: t}
	} else if t.retired.Load() {
		t.CloseIdleConnections()
	}
	return resp, err
}

type retiringBody struct {
	io.ReadCloser
	transport *proxyTransportEntry
}

func (b *retiringBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	// Cleanup on EOF/error as well as Close; net/http may pool at EOF.
	if err != nil && b.transport.retired.Load() {
		b.transport.CloseIdleConnections()
	}
	return n, err
}
func (b *retiringBody) Close() error {
	err := b.ReadCloser.Close()
	if b.transport.retired.Load() {
		b.transport.CloseIdleConnections()
	}
	return err
}
