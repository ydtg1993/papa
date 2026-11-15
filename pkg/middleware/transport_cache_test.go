package middleware

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type cacheTestTransport struct{ closes atomic.Int64 }

func (t *cacheTestTransport) CloseIdleConnections() { t.closes.Add(1) }
func (*cacheTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("payload"))}, nil
}
func fillCache(c *ProxyTransportCache) {
	for i := 0; i < ProxyTransportCacheCapacity; i++ {
		c.Get(fmt.Sprint(i), func() http.RoundTripper { return &cacheTestTransport{} })
	}
}
func TestProxyTransportCacheLRUAndCapacity(t *testing.T) {
	var c ProxyTransportCache
	first, second := &cacheTestTransport{}, &cacheTestTransport{}
	a := c.Get("0", func() http.RoundTripper { return first })
	c.Get("1", func() http.RoundTripper { return second })
	for i := 2; i < ProxyTransportCacheCapacity; i++ {
		c.Get(fmt.Sprint(i), func() http.RoundTripper { return &cacheTestTransport{} })
	}
	if c.Get("0", nil) != a {
		t.Fatal("hit did not reuse")
	}
	c.Get("new", func() http.RoundTripper { return &cacheTestTransport{} })
	if len(c.entries) != ProxyTransportCacheCapacity || c.entries["1"] != nil || c.entries["0"] == nil {
		t.Fatal("LRU capacity/order")
	}
	if second.closes.Load() != 1 || first.closes.Load() != 0 {
		t.Fatal("wrong transport retired")
	}
}
func TestProxyTransportCacheRetiredClientCannotLeaveIdleConnections(t *testing.T) {
	var c ProxyTransportCache
	base := &cacheTestTransport{}
	tr := c.Get("old", func() http.RoundTripper { return base })
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	response, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	fillCache(&c)
	if base.closes.Load() != 1 {
		t.Fatal("eviction did not close idle")
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "payload" {
		t.Fatal("active body interrupted", err)
	}
	response.Body.Close()
	before := base.closes.Load()
	// A long-lived file download may retain its old client after eviction.
	response, err = tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if base.closes.Load() <= before {
		t.Fatal("retired client recreated idle connections")
	}
}
func TestProxyTransportCacheConcurrentChurn(t *testing.T) {
	var c ProxyTransportCache
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				tr := c.Get(fmt.Sprintf("%d-%d", g, i), func() http.RoundTripper { return &cacheTestTransport{} })
				req, _ := http.NewRequest("GET", "http://example.invalid", nil)
				resp, err := tr.RoundTrip(req)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				c.mu.Lock()
				n := len(c.entries)
				c.mu.Unlock()
				if n > ProxyTransportCacheCapacity {
					t.Error("capacity exceeded")
				}
			}
		}(g)
	}
	wg.Wait()
}
func TestProxyTransportEvictionDoesNotInterruptStreamingResponse(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("last"))
	}))
	defer srv.Close()
	var c ProxyTransportCache
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	client := &http.Client{Transport: c.Get("stream", func() http.RoundTripper { return base })}
	resp, err := client.Get(srv.URL)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	fillCache(&c)
	close(release)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(data) != "firstlast" {
		t.Fatal("eviction interrupted stream", string(data), err)
	}
}

type cacheHookTransport struct {
	trip  func(*http.Request) (*http.Response, error)
	close func()
}

func (t *cacheHookTransport) RoundTrip(r *http.Request) (*http.Response, error) { return t.trip(r) }
func (t *cacheHookTransport) CloseIdleConnections()                             { t.close() }

func TestProxyTransportCacheConcurrentMissCreatesOnce(t *testing.T) {
	var c ProxyTransportCache
	var made atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan http.RoundTripper, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- c.Get("same", func() http.RoundTripper { made.Add(1); return &cacheTestTransport{} })
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first http.RoundTripper
	for tr := range results {
		if first == nil {
			first = tr
		}
		if tr != first {
			t.Fatal("duplicate transport")
		}
	}
	if made.Load() != 1 {
		t.Fatal("factory called", made.Load())
	}
}

func TestProxyTransportEvictionCleanupDoesNotHoldCacheLock(t *testing.T) {
	var c ProxyTransportCache
	entered, release := make(chan struct{}), make(chan struct{})
	c.Get("old", func() http.RoundTripper { return &cacheHookTransport{close: func() { close(entered); <-release }} })
	for i := 0; i < ProxyTransportCacheCapacity-1; i++ {
		c.Get(fmt.Sprint(i), func() http.RoundTripper { return &cacheTestTransport{} })
	}
	evicted := make(chan struct{})
	defer func() { close(release); <-evicted }()
	go func() {
		c.Get("replacement", func() http.RoundTripper { return &cacheTestTransport{} })
		close(evicted)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("eviction never reached closer")
	}
	hit := make(chan struct{})
	go func() { c.Get("replacement", nil); close(hit) }()
	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup held cache lock")
	}
	// release is deferred so failure paths also let the eviction goroutine exit.
}

func TestProxyTransportEvictedBeforeHeadersAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var c ProxyTransportCache
			started, release := make(chan struct{}), make(chan struct{})
			var closed atomic.Int64
			want := errors.New("request cancelled")
			tr := c.Get("old", func() http.RoundTripper {
				return &cacheHookTransport{
					close: func() { closed.Add(1) },
					trip: func(*http.Request) (*http.Response, error) {
						close(started)
						<-release
						if failed {
							return nil, want
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
					},
				}
			})
			done := make(chan error, 1)
			go func() {
				req, _ := http.NewRequest("GET", "http://example.invalid", nil)
				resp, err := tr.RoundTrip(req)
				if resp != nil {
					_, err = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
				done <- err
			}()
			<-started
			fillCache(&c)
			close(release)
			err := <-done
			if failed && !errors.Is(err, want) || !failed && err != nil {
				t.Fatal(err)
			}
			if closed.Load() < 2 {
				t.Fatal("no post-request cleanup", closed.Load())
			}
		})
	}
}

func TestProxyTransportOldBodyDoesNotRetireReplacement(t *testing.T) {
	var c ProxyTransportCache
	oldBase, newBase := &cacheTestTransport{}, &cacheTestTransport{}
	old := c.Get("same", func() http.RoundTripper { return oldBase })
	req, _ := http.NewRequest("GET", "http://example.invalid", nil)
	resp, err := old.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	fillCache(&c)
	replacement := c.Get("same", func() http.RoundTripper { return newBase })
	// EOF alone performs late cleanup, even before the caller closes the body.
	before := oldBase.closes.Load()
	if _, err = io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if oldBase.closes.Load() <= before {
		t.Fatal("EOF did not clean retired transport")
	}
	resp.Body.Close()
	if newBase.closes.Load() != 0 || c.Get("same", nil) != replacement {
		t.Fatal("old response affected replacement")
	}
}
