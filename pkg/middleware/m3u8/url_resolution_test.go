package m3u8

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/egress"
)

type referenceRequests struct {
	mu   sync.Mutex
	URIs []string
}

func referenceServer(t *testing.T, handle http.HandlerFunc) (*httptest.Server, *referenceRequests) {
	t.Helper()
	requests := &referenceRequests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.mu.Lock()
		requests.URIs = append(requests.URIs, r.URL.RequestURI())
		requests.mu.Unlock()
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func (r *referenceRequests) require(t *testing.T, want ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !reflect.DeepEqual(r.URIs, want) {
		t.Errorf("requested paths = %q, want %q", r.URIs, want)
	}
}

func requireReferenceDownload(t *testing.T, playlistURL string, extraOrigins ...string) {
	t.Helper()
	cfg := DefaultConfig()
	u, err := url.Parse(playlistURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network = egress.Config{PrivateOrigins: append(extraOrigins, u.Scheme+"://"+u.Host)}
	cfg.OutputDir = t.TempDir()
	cfg.ResumeStateDir = t.TempDir()
	cfg.MaxConcurrent = 1
	cfg.MaxRetries = 0
	cfg.EnableResume = false
	cfg.AutoMerge = false // Compare exact bytes without ffmpeg.
	d := NewDownloader(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := d.Download(ctx, playlistURL, "", "out.ts")
	if result.Error != nil {
		t.Errorf("Download: %v", result.Error)
		return
	}
	got, err := os.ReadFile(filepath.Join(cfg.OutputDir, result.OutputFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "CORRECT" {
		t.Errorf("Download returned success with output %q, want CORRECT", got)
	}
}

// Wrong routes deliberately succeed: HTTP status alone cannot detect a bad base.
func TestDownloadRedirectedMediaReference(t *testing.T) {
	server, requests := referenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/start/list.m3u8":
			http.Redirect(w, r, "/final/list.m3u8", http.StatusFound)
		case "/final/list.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\ngood.ts\n#EXT-X-ENDLIST\n")
		case "/final/good.ts":
			fmt.Fprint(w, "CORRECT")
		default:
			fmt.Fprint(w, "WRONG")
		}
	})
	requireReferenceDownload(t, server.URL+"/start/list.m3u8")
	requests.require(t, "/start/list.m3u8", "/final/list.m3u8", "/final/good.ts")
}

func TestDownloadRedirectedMasterAndMediaReferences(t *testing.T) {
	server, requests := referenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/start/master.m3u8":
			http.Redirect(w, r, "/catalog/master.m3u8?list=1", http.StatusFound)
		case "/catalog/master.m3u8?list=1":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nrenditions/media.m3u8\n")
		case "/catalog/renditions/media.m3u8":
			http.Redirect(w, r, "/assets/final/media.m3u8?list=2", http.StatusTemporaryRedirect)
		case "/assets/final/media.m3u8?list=2":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\n../chunks/good.ts\n#EXT-X-ENDLIST\n")
		case "/assets/chunks/good.ts":
			fmt.Fprint(w, "CORRECT")
		default:
			fmt.Fprint(w, "WRONG")
		}
	})
	requireReferenceDownload(t, server.URL+"/start/master.m3u8")
	requests.require(t, "/start/master.m3u8", "/catalog/master.m3u8?list=1", "/catalog/renditions/media.m3u8", "/assets/final/media.m3u8?list=2", "/assets/chunks/good.ts")
}

func TestDownloadNetworkPathReferencesAcrossPorts(t *testing.T) {
	for _, kind := range []string{"segment", "variant", "key", "map"} {
		t.Run(kind, func(t *testing.T) {
			key := []byte("0123456789abcdef")
			encrypted := encryptAES128CBC(t, []byte("CORRECT"), key, ivForSeq(0))
			remote, remoteRequests := referenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.RequestURI() {
				case "/cross/media.m3u8":
					fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n")
				case "/cross/segment.ts":
					fmt.Fprint(w, "CORRECT")
				case "/cross/key.bin":
					_, _ = w.Write(key)
				case "/cross/init.mp4":
					fmt.Fprint(w, "COR")
				default:
					fmt.Fprint(w, "WRONG")
				}
			})
			remoteRef := strings.TrimPrefix(remote.URL, "http:") + "/cross/"
			var playlist string
			var wantOrigin, wantRemote []string
			wantOrigin = []string{"/start/list.m3u8"}
			switch kind {
			case "segment":
				playlist = "#EXTM3U\n#EXTINF:1,\n" + remoteRef + "segment.ts\n#EXT-X-ENDLIST\n"
				wantRemote = []string{"/cross/segment.ts"}
			case "variant":
				playlist = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n" + remoteRef + "media.m3u8\n"
				wantRemote = []string{"/cross/media.m3u8", "/cross/segment.ts"}
			case "key":
				playlist = "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"" + remoteRef + "key.bin\"\n#EXTINF:1,\nencrypted.ts\n#EXT-X-ENDLIST\n"
				wantOrigin = append(wantOrigin, "/start/encrypted.ts")
				wantRemote = []string{"/cross/key.bin"}
			case "map":
				playlist = "#EXTM3U\n#EXT-X-MAP:URI=\"" + remoteRef + "init.mp4\"\n#EXTINF:1,\ntail.ts\n#EXT-X-ENDLIST\n"
				wantOrigin = append(wantOrigin, "/start/tail.ts")
				wantRemote = []string{"/cross/init.mp4"}
			}
			origin, originRequests := referenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.RequestURI() {
				case "/start/list.m3u8":
					fmt.Fprint(w, playlist)
				case "/start/encrypted.ts":
					_, _ = w.Write(encrypted)
				case "/start/tail.ts":
					fmt.Fprint(w, "RECT")
				default:
					fmt.Fprint(w, "WRONG")
				}
			})
			requireReferenceDownload(t, origin.URL+"/start/list.m3u8", remote.URL)
			originRequests.require(t, wantOrigin...)
			remoteRequests.require(t, wantRemote...)
		})
	}
}

func TestDownloadPlaylistReferenceSemantics(t *testing.T) {
	for _, tc := range []struct{ name, playlistURI, reference, segmentURI string }{
		{"relative", "/dir/list.m3u8?list=1", "good.ts", "/dir/good.ts"},
		{"parent", "/dir/sub/list.m3u8", "../good.ts", "/dir/good.ts"},
		{"root", "/dir/list.m3u8?list=1", "/root/good.ts", "/root/good.ts"},
		{"query", "/dir/list.m3u8?list=1", "?part=segment&sig=a%2Fb+z", "/dir/list.m3u8?part=segment&sig=a%2Fb+z"},
		{"escaped-directory", "/escaped%2Fdir/list.m3u8?list=1", "part%2Fone.ts?sig=a%2Fb+z", "/escaped%2Fdir/part%2Fone.ts?sig=a%2Fb+z"},
		{"escaped-dot", "/dir/list.m3u8", "%2e%2e/good.ts", "/dir/%2e%2e/good.ts"},
		{"fragment", "/dir/list.m3u8", "good.ts#part", "/dir/good.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, requests := referenceServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.RequestURI() {
				case tc.playlistURI:
					fmt.Fprintf(w, "#EXTM3U\n#EXTINF:1,\n%s\n#EXT-X-ENDLIST\n", tc.reference)
				case tc.segmentURI:
					fmt.Fprint(w, "CORRECT")
				default:
					fmt.Fprint(w, "WRONG")
				}
			})
			requireReferenceDownload(t, server.URL+tc.playlistURI)
			requests.require(t, tc.playlistURI, tc.segmentURI)
		})
	}
}

func TestDownloadRedirectedKeyAndMapReferences(t *testing.T) {
	key := []byte("0123456789abcdef")
	encrypted := encryptAES128CBC(t, []byte("RECT"), key, ivForSeq(0))
	server, requests := referenceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/start/list.m3u8":
			http.Redirect(w, r, "/final%2Fdir/list.m3u8?list=1", http.StatusFound)
		case "/final%2Fdir/list.m3u8?list=1":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MAP:URI=\"?part=init&sig=a%2Fb+z\"\n"+
				"#EXT-X-KEY:METHOD=AES-128,URI=\"../keys/key%2Fname.bin?sig=a%2Fb+z\"\n"+
				"#EXTINF:1,\nsegment%2Fone.ts?sig=a%2Fb+z\n#EXT-X-ENDLIST\n")
		case "/final%2Fdir/list.m3u8?part=init&sig=a%2Fb+z":
			fmt.Fprint(w, "COR")
		case "/keys/key%2Fname.bin?sig=a%2Fb+z":
			_, _ = w.Write(key)
		case "/final%2Fdir/segment%2Fone.ts?sig=a%2Fb+z":
			_, _ = w.Write(encrypted)
		default:
			fmt.Fprint(w, "WRONG")
		}
	})
	requireReferenceDownload(t, server.URL+"/start/list.m3u8")
	requests.require(t, "/start/list.m3u8", "/final%2Fdir/list.m3u8?list=1", "/final%2Fdir/list.m3u8?part=init&sig=a%2Fb+z", "/final%2Fdir/segment%2Fone.ts?sig=a%2Fb+z", "/keys/key%2Fname.bin?sig=a%2Fb+z")
}
