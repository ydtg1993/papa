package fetchruntime

import (
	"context"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"sync"
	"testing"
)

func TestResourcesConcurrentRegistrationAndLookup(t *testing.T) {
	var r Resources
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.SetHTML(&htmlfetch.Client{})
				r.HTML()
				r.SetAccount(nil)
				r.Account()
				r.SetBrowser(nil)
				r.Browser()
				r.SetIdentity(nil)
				r.Identity()
			}
		}()
	}
	wg.Wait()
}
func TestResourcesUnconfiguredAndUnpooledAcquire(t *testing.T) {
	var r Resources
	ctx := context.Background()
	if got, err := r.PrepareAccount(ctx, "s", ""); err != nil || got != ctx {
		t.Fatal(err)
	}
	if _, err := r.PrepareAccount(ctx, "s", "p"); err == nil {
		t.Fatal("missing account accepted")
	}
	if _, err := r.Render(ctx, "https://example.invalid", browser.PageOptions{}); err == nil {
		t.Fatal("missing browser accepted")
	}
}
func TestDownloadsResolvedFallbackSnapshot(t *testing.T) {
	var d Downloads
	m := m3u8.NewDownloader(m3u8.DefaultConfig())
	f := filedown.NewDownloader(filedown.DefaultConfig())
	d.Register("site", SiteDownloads{})
	d.Resolve([]StageDownloads{{Site: "site", Stage: "detail"}}, m, f)
	if d.M3U8Stage("site", "detail", m) != m || d.FileSite("site", f) != f {
		t.Fatal("zero overlay created another resource")
	}
}
