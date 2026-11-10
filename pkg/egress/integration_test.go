package egress_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v4/pkg/notify"
)

func TestFrameworkExitsDefaultDenyAndExplicitPrivateGrant(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); fmt.Fprint(w, "fixture") }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := htmlfetch.NewClient(htmlfetch.Config{}).Fetch(ctx, srv.URL); err == nil {
		t.Fatal("HTML default allowed private origin")
	}
	cfg := filedown.DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.MaxRetries = 0
	d := filedown.NewDownloader(cfg)
	if result := d.Download(ctx, srv.URL, "", "file"); result.Error == nil {
		t.Fatal("HEAD/download default allowed private origin")
	}
	d.SetClient(srv.Client())
	if result := d.Download(ctx, srv.URL, "", "direct", &filedown.DownloadOptions{Direct: true}); result.Error == nil {
		t.Fatal("SetClient bypassed network policy")
	}
	w := notify.NewWebhook(srv.URL)
	if err := w.Notify(ctx, core.AlertEvent{}); err == nil {
		t.Fatal("webhook default allowed private origin")
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked services contacted target %d times", hits.Load())
	}
	network := egress.Config{PrivateOrigins: []string{srv.URL}}
	if _, err := htmlfetch.NewClient(htmlfetch.Config{Network: network}).Fetch(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	cfg.Network = network
	d = filedown.NewDownloader(cfg)
	if result := d.With(filedown.Overlay{}).Download(ctx, srv.URL, "", "allowed", &filedown.DownloadOptions{Direct: true}); result.Error != nil {
		t.Fatal(result.Error)
	}
	w.Network = network
	if err := w.Notify(ctx, core.AlertEvent{}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("authorized request count=%d", hits.Load())
	}
}
func TestM3U8RechecksVariantSegmentKeyAndMapOrigins(t *testing.T) {
	for _, kind := range []string{"variant", "segment", "key", "map"} {
		t.Run(kind, func(t *testing.T) {
			var hits atomic.Int32
			denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); fmt.Fprint(w, "private-body") }))
			defer denied.Close()
			main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/part.ts" {
					fmt.Fprint(w, "content")
					return
				}
				switch kind {
				case "variant":
					fmt.Fprintf(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n%s/list.m3u8\n", denied.URL)
				case "segment":
					fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1,\n%s/part.ts\n#EXT-X-ENDLIST\n", denied.URL)
				case "key":
					fmt.Fprintf(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"%s/key\"\n#EXTINF:1,\npart.ts\n#EXT-X-ENDLIST\n", denied.URL)
				case "map":
					fmt.Fprintf(w, "#EXTM3U\n#EXT-X-MAP:URI=\"%s/init.mp4\"\n#EXTINF:1,\npart.ts\n#EXT-X-ENDLIST\n", denied.URL)
				}
			}))
			defer main.Close()
			cfg := m3u8.DefaultConfig()
			cfg.Network = egress.Config{PrivateOrigins: []string{main.URL}}
			cfg.OutputDir = t.TempDir()
			cfg.ResumeStateDir = t.TempDir()
			cfg.MaxRetries = 0
			cfg.AutoMerge = false
			cfg.EnableResume = false
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := m3u8.NewDownloader(cfg).With(m3u8.Overlay{}).Download(ctx, main.URL+"/list.m3u8", "", "video.ts")
			if result.Error == nil {
				t.Fatalf("%s foreign origin accepted", kind)
			}
			if hits.Load() != 0 {
				t.Fatalf("%s contacted denied target", kind)
			}
		})
	}
}

func TestDownloaderConfigCopiesCannotExpandPrivateGrant(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\npart.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()
	f := filedown.DefaultConfig()
	f.OutputDir, f.ResumeStateDir, f.MaxRetries = t.TempDir(), t.TempDir(), 0
	f.Network.PrivateOrigins = []string{"http://127.0.0.1:1"}
	fd := filedown.NewDownloader(f)
	f.Network.PrivateOrigins[0] = srv.URL
	fc := fd.Config()
	fc.Network.PrivateOrigins[0] = srv.URL
	fd.SetClient(srv.Client())
	m := m3u8.DefaultConfig()
	m.OutputDir, m.ResumeStateDir, m.MaxRetries = t.TempDir(), t.TempDir(), 0
	m.Network.PrivateOrigins = []string{"http://127.0.0.1:1"}
	md := m3u8.NewDownloader(m)
	m.Network.PrivateOrigins[0] = srv.URL
	mc := md.Config()
	mc.Network.PrivateOrigins[0] = srv.URL
	md.SetClient(srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, dl := range []*filedown.Downloader{fd, fd.With(filedown.Overlay{})} {
		if r := dl.Download(ctx, srv.URL, "", "file", &filedown.DownloadOptions{Direct: true}); r.Error == nil {
			t.Fatal("file grant expanded")
		}
	}
	for _, dl := range []*m3u8.Downloader{md, md.With(m3u8.Overlay{})} {
		if r := dl.Download(ctx, srv.URL, "", "video.ts"); r.Error == nil {
			t.Fatal("m3u8 grant expanded")
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("config mutation contacted denied target %d times", hits.Load())
	}
}

func TestDownloaderConfigCopiesCannotExpandAllowedGrant(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\npart.ts\n#EXT-X-ENDLIST\n")
	}))
	defer srv.Close()
	f := filedown.DefaultConfig()
	f.OutputDir, f.ResumeStateDir, f.MaxRetries = t.TempDir(), t.TempDir(), 0
	f.Network = egress.Config{PrivateOrigins: []string{srv.URL}, AllowedOrigins: []string{"http://127.0.0.1:1"}}
	fd := filedown.NewDownloader(f)
	f.Network.AllowedOrigins[0] = srv.URL
	fc := fd.Config()
	fc.Network.AllowedOrigins[0] = srv.URL
	fd.SetClient(srv.Client())
	m := m3u8.DefaultConfig()
	m.OutputDir, m.ResumeStateDir, m.MaxRetries = t.TempDir(), t.TempDir(), 0
	m.Network = egress.Config{PrivateOrigins: []string{srv.URL}, AllowedOrigins: []string{"http://127.0.0.1:1"}}
	md := m3u8.NewDownloader(m)
	m.Network.AllowedOrigins[0] = srv.URL
	mc := md.Config()
	mc.Network.AllowedOrigins[0] = srv.URL
	md.SetClient(srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, dl := range []*filedown.Downloader{fd, fd.With(filedown.Overlay{})} {
		if result := dl.Download(ctx, srv.URL, "", "file", &filedown.DownloadOptions{Direct: true}); result.Error == nil {
			t.Fatal("file allowlist expanded")
		}
	}
	for _, dl := range []*m3u8.Downloader{md, md.With(m3u8.Overlay{})} {
		if result := dl.Download(ctx, srv.URL, "", "video.ts"); result.Error == nil {
			t.Fatal("m3u8 allowlist expanded")
		}
	}
	if hits.Load() != 0 {
		t.Fatal("forbidden origin reached", hits.Load())
	}
}

func TestHTMLHotReloadCannotGrantPrivateOrigin(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := htmlfetch.NewClient(htmlfetch.Config{})
	c.SetConfig(htmlfetch.Config{Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if _, err := c.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("hot reload expanded private grant")
	}
	if hits.Load() != 0 {
		t.Fatal("denied origin contacted")
	}
}

func TestInvalidDownloaderProxyNeverFallsBackToDirect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	f := filedown.DefaultConfig()
	f.OutputDir, f.ResumeStateDir, f.MaxRetries = t.TempDir(), t.TempDir(), 0
	f.Network.PrivateOrigins = []string{srv.URL}
	m := m3u8.DefaultConfig()
	m.OutputDir, m.ResumeStateDir, m.MaxRetries = t.TempDir(), t.TempDir(), 0
	m.Network.PrivateOrigins = []string{srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if r := filedown.NewDownloader(f).Download(ctx, srv.URL, "", "file", &filedown.DownloadOptions{Direct: true, Proxy: "://invalid"}); r.Error == nil {
		t.Fatal("file proxy fell back to direct")
	}
	if r := m3u8.NewDownloader(m).Download(ctx, srv.URL, "", "video.ts", &m3u8.DownloadOptions{Proxy: "://invalid"}); r.Error == nil {
		t.Fatal("m3u8 proxy fell back to direct")
	}
	if hits.Load() != 0 {
		t.Fatal("invalid proxy contacted target directly")
	}
}
