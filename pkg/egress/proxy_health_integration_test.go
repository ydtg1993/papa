package egress_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

func TestFrameworkFixedProxyHealthStopsHTMLFileAndPlaylistRequests(t *testing.T) {
	for _, kind := range []string{"html", "file", "m3u8"} {
		t.Run(kind, func(t *testing.T) {
			var targets, proxies atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targets.Add(1) }))
			defer target.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxies.Add(1)
				w.WriteHeader(http.StatusProxyAuthRequired)
			}))
			defer upstream.Close()
			manager := proxy.NewManager("", 0)
			defer manager.Close()
			health := proxy.DefaultHealthConfig()
			health.Enabled, health.FailureThreshold = true, 1
			if err := manager.ConfigureHealth(health); err != nil {
				t.Fatal(err)
			}
			network := egress.Config{PrivateOrigins: []string{target.URL}}
			var request func(string) error
			switch kind {
			case "html":
				client := htmlfetch.NewClient(htmlfetch.Config{Network: network, ProxyManager: manager})
				request = func(string) error {
					_, err := client.Fetch(core.WithProxyURL(context.Background(), upstream.URL), target.URL)
					return err
				}
			case "file":
				cfg := filedown.DefaultConfig()
				cfg.Network, cfg.ProxyManager = network, manager
				cfg.OutputDir, cfg.ResumeStateDir, cfg.MaxRetries = t.TempDir(), t.TempDir(), 0
				client := filedown.NewDownloader(cfg).With(filedown.Overlay{})
				request = func(name string) error {
					return client.Download(context.Background(), target.URL, "", name, &filedown.DownloadOptions{Proxy: upstream.URL, Direct: true}).Error
				}
			case "m3u8":
				cfg := m3u8.DefaultConfig()
				cfg.Network, cfg.ProxyManager = network, manager
				cfg.OutputDir, cfg.ResumeStateDir, cfg.MaxRetries = t.TempDir(), t.TempDir(), 0
				cfg.OutputMode, cfg.EnableResume = m3u8.OutputPlaylist, false
				client := m3u8.NewDownloader(cfg).With(m3u8.Overlay{})
				request = func(name string) error {
					return client.Download(context.Background(), target.URL, "", name, &m3u8.DownloadOptions{Proxy: upstream.URL}).Error
				}
			}
			if err := request("first"); err == nil {
				t.Fatal("proxy authentication failure succeeded")
			}
			if state := manager.Snapshot(); len(state) != 1 || state[0].State != proxy.StateCooling || state[0].InFlight != 0 {
				t.Fatalf("actual client did not report proxy failure: %+v", state)
			}
			if err := request("second"); err == nil {
				t.Fatal("cooled fixed proxy request succeeded")
			}
			if proxies.Load() != 1 || targets.Load() != 0 {
				t.Fatalf("request bypassed health or went direct: proxies=%d targets=%d", proxies.Load(), targets.Load())
			}
		})
	}
}
