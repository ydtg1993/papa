package m3u8

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
	"net/http"
	"testing"
)

func TestProxyCacheSharedBoundAndEviction(t *testing.T) {
	d := NewDownloader(nil)
	lookup := func(d *Downloader, addr string) *http.Client { return d.clientFor(&DownloadOptions{Proxy: addr}) }
	first := lookup(d, "http://127.0.0.1:1000")
	derived := d.With(Overlay{}).WithResources(middleware.DownloadLimits{}, nil)
	if lookup(derived, "http://127.0.0.1:1000").Transport != first.Transport {
		t.Fatal("With did not share cache")
	}
	for i := 0; i < middleware.ProxyTransportCacheCapacity; i++ {
		lookup(derived, fmt.Sprintf("http://127.0.0.1:%d", 2000+i))
	}
	if lookup(d, "http://127.0.0.1:1000").Transport == first.Transport {
		t.Fatal("cache did not evict oldest across With")
	}
	if lookup(NewDownloader(nil), "http://127.0.0.1:1000").Transport == lookup(d, "http://127.0.0.1:1000").Transport {
		t.Fatal("independent downloader shared cache")
	}
	// Invalid addresses are bounded by the same cache, never a separate map.
	bad := lookup(d, "%bad").Transport
	for i := 0; i < middleware.ProxyTransportCacheCapacity; i++ {
		lookup(derived, fmt.Sprintf("%%bad%d", i))
	}
	if lookup(d, "%bad").Transport == bad {
		t.Fatal("invalid proxies bypassed eviction")
	}
}
