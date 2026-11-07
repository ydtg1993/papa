package m3u8

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func remediationConfig(t *testing.T, origin string) *Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Network.PrivateOrigins = []string{origin}
	cfg.OutputDir, cfg.ResumeStateDir = t.TempDir(), t.TempDir()
	cfg.AutoMerge, cfg.MaxRetries, cfg.MaxConcurrent = false, 0, 1
	return cfg
}

func TestResumePathsCannotReadMoveOrDeleteExternalFiles(t *testing.T) {
	for _, mode := range []OutputMode{OutputMerge, OutputSegments} {
		t.Run(mode.String(), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\npart.ts\n#EXT-X-ENDLIST\n")
			}))
			defer srv.Close()
			cfg := remediationConfig(t, srv.URL)
			d := NewDownloader(cfg)
			la := &labor{source: srv.URL + "/list.m3u8", filename: "movie.ts"}
			external := filepath.Join(t.TempDir(), "secret.ts")
			if err := os.WriteFile(external, []byte("external secret"), 0600); err != nil {
				t.Fatal(err)
			}
			canonical := filepath.Join(d.segDir(la), "segment_00000.ts")
			if err := os.MkdirAll(filepath.Dir(canonical), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(canonical, []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := d.saveResumeState(la, &ResumeState{TotalSegments: 1, Completed: []int{0}, SegmentFiles: []string{external}}); err != nil {
				t.Fatal(err)
			}
			res := d.Download(context.Background(), la.source, "", la.filename, &DownloadOptions{Output: &mode})
			if res.Error != nil {
				t.Fatal(res.Error)
			}
			if res.Size != 4 {
				t.Errorf("size used untrusted resume path: got %d, want 4", res.Size)
			}
			out := filepath.Join(cfg.OutputDir, res.OutputFile)
			if mode == OutputSegments {
				out = filepath.Join(out, "segment_00000.ts")
			}
			if b, err := os.ReadFile(out); err != nil || string(b) != "safe" {
				t.Errorf("output used untrusted resume path: %q, %v", b, err)
			}
			if b, err := os.ReadFile(external); err != nil || string(b) != "external secret" {
				t.Errorf("external file moved/deleted/changed: %q, %v", b, err)
			}
		})
	}
}

func TestMergeExtensionValidatedAtEffectiveEntry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Error(w, "unexpected request", 500) }))
	defer srv.Close()
	for _, ext := range []string{"../evil", ".mp4/evil", ".mp4\\evil", "mp4", ".", ".mp4\nfile", "." + strings.Repeat("a", 17)} {
		for _, entry := range []string{"config", "overlay", "options"} {
			t.Run(entry+"/"+ext, func(t *testing.T) {
				cfg := remediationConfig(t, srv.URL)
				cfg.AutoMerge = true
				if entry == "config" {
					cfg.MergeOutputExt = ext
				}
				d := NewDownloader(cfg)
				if entry == "overlay" {
					d = d.With(Overlay{MergeExt: &ext})
				}
				var opt *DownloadOptions
				if entry == "options" {
					opt = &DownloadOptions{MergeExt: &ext}
				}
				res := d.Download(context.Background(), srv.URL, "", "movie.ts", opt)
				if res.Error == nil || !strings.Contains(res.Error.Error(), "invalid merge extension") {
					t.Errorf("invalid extension not rejected at %s: %v", entry, res.Error)
				}
			})
		}
	}
	if hits.Load() != 0 {
		t.Errorf("invalid extensions triggered %d HTTP requests", hits.Load())
	}
}

func TestResumeCompletedMissingIndexedFileRejectsAndRecovers(t *testing.T) {
	for _, storedPath := range []string{"empty", "external"} {
		for _, mode := range []OutputMode{OutputMerge, OutputSegments} {
			t.Run(storedPath+"/"+mode.String(), func(t *testing.T) {
				var segmentHits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/list.m3u8" {
						fmt.Fprint(w, "#EXTM3U\n#EXTINF:1,\npart.ts\n#EXT-X-ENDLIST\n")
						return
					}
					segmentHits.Add(1)
					fmt.Fprint(w, "fresh media")
				}))
				defer srv.Close()
				cfg := remediationConfig(t, srv.URL)
				d := NewDownloader(cfg)
				la := &labor{source: srv.URL + "/list.m3u8", filename: "movie.ts"}
				external := filepath.Join(t.TempDir(), "secret.ts")
				if err := os.WriteFile(external, []byte("external secret"), 0600); err != nil {
					t.Fatal(err)
				}
				stored := ""
				if storedPath == "external" {
					stored = external
				}
				if err := d.saveResumeState(la, &ResumeState{TotalSegments: 1, Completed: []int{0}, SegmentFiles: []string{stored}}); err != nil {
					t.Fatal(err)
				}
				opt := &DownloadOptions{Output: &mode}
				res := d.Download(context.Background(), la.source, "", la.filename, opt)
				if res.Error == nil || !strings.Contains(res.Error.Error(), "segment file missing") {
					t.Fatalf("completed marker with no indexed file must fail: %+v", res)
				}
				if _, err := os.Stat(d.getStateFilePath(la)); !os.IsNotExist(err) {
					t.Fatalf("stale state must be removed: %v", err)
				}
				res = d.Download(context.Background(), la.source, "", la.filename, opt)
				if res.Error != nil {
					t.Fatal(res.Error)
				}
				out := filepath.Join(cfg.OutputDir, res.OutputFile)
				if mode == OutputSegments {
					out = filepath.Join(out, "segment_00000.ts")
				}
				if data, err := os.ReadFile(out); err != nil || string(data) != "fresh media" {
					t.Fatalf("retry output: %q, %v", data, err)
				}
				if res.Size != 11 || res.Segments != 1 || segmentHits.Load() != 1 {
					t.Fatalf("retry result: %+v, segment requests: %d", res, segmentHits.Load())
				}
				if data, err := os.ReadFile(external); err != nil || string(data) != "external secret" {
					t.Fatalf("external file changed: %q, %v", data, err)
				}
			})
		}
	}
}

func TestPlaylistRejectsUnsupportedKeyMethodAndDanglingRange(t *testing.T) {
	d := NewDownloader(nil)
	for name, playlist := range map[string]string{
		"sample-aes":      "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key\"\npart.ts\n",
		"unknown-method":  "#EXTM3U\n#EXT-X-KEY:METHOD=UNKNOWN,URI=\"key\"\npart.ts\n",
		"missing-method":  "#EXTM3U\n#EXT-X-KEY:URI=\"key\"\npart.ts\n",
		"missing-key-uri": "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128\npart.ts\n",
		"range-eof":       "#EXTM3U\npart.ts\n#EXT-X-BYTERANGE:4@0\n",
		"range-endlist":   "#EXTM3U\npart.ts\n#EXT-X-BYTERANGE:4@0\n#EXT-X-ENDLIST\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := d.parsePlaylistEnhancedWithIndex(playlist, "https://example.com/list.m3u8")
			if err == nil {
				t.Fatal("malformed/unsupported playlist silently accepted")
			}
		})
	}
	_, _, keys, err := d.parsePlaylistEnhancedWithIndex("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\nfirst.ts\n#EXT-X-KEY:METHOD=NONE\nsecond.ts\n", "https://example.com/list.m3u8")
	if err != nil || len(keys) != 2 || keys[0] == nil || keys[1] != nil {
		t.Fatalf("METHOD=NONE must clear encryption: %v, %v", keys, err)
	}
}

func TestKeyCacheDoesNotCrossDownloads(t *testing.T) {
	var generation atomic.Int32
	var keyHits atomic.Int32
	keys := [][]byte{[]byte("0123456789abcdef"), []byte("fedcba9876543210")}
	plain := []byte("media bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keys[generation.Load()]
		switch r.URL.Path {
		case "/list.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\",IV=0x00000000000000000000000000000000\npart.ts\npart.ts\n#EXT-X-ENDLIST\n")
		case "/key":
			keyHits.Add(1)
			w.Write(key)
		default:
			padding := aes.BlockSize - len(plain)%aes.BlockSize
			data := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(padding)}, padding)...)
			block, _ := aes.NewCipher(key)
			cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(data, data)
			w.Write(data)
		}
	}))
	defer srv.Close()
	cfg := remediationConfig(t, srv.URL)
	d := NewDownloader(cfg)
	for i := 0; i < 2; i++ {
		generation.Store(int32(i))
		res := d.Download(context.Background(), srv.URL+"/list.m3u8", "", fmt.Sprintf("movie%d.ts", i))
		if res.Error != nil {
			t.Fatalf("download %d reused stale key: %v", i, res.Error)
		}
		if b, err := os.ReadFile(filepath.Join(cfg.OutputDir, res.OutputFile)); err != nil || !bytes.Equal(b, bytes.Repeat(plain, 2)) {
			t.Fatalf("download %d plaintext: %q, %v", i, b, err)
		}
	}
	if keyHits.Load() != 2 {
		t.Fatalf("expected one key fetch per download, got %d", keyHits.Load())
	}
}

func TestFFmpegConcatHandlesApostrophePaths(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_FFMPEG_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_FFMPEG_BIN not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := DefaultConfig()
	cfg.ResumeStateDir, cfg.FfmpegPath = t.TempDir(), bin
	part := filepath.Join(t.TempDir(), "part'quoted.ts")
	if out, err := exec.CommandContext(ctx, bin, "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-c:a", "aac", "-f", "mpegts", part).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	d := NewDownloader(cfg)
	for _, init := range []bool{false, true} {
		var initPath string
		parts := []string{part}
		if init {
			initPath, parts = part, nil
		}
		if err := d.mergeToMP4(ctx, initPath, parts, filepath.Join(t.TempDir(), "ok.mp4")); err != nil {
			t.Errorf("init=%v: %v", init, err)
		}
	}
}

func TestConcurrentDownloadsKeepCredentialSpecificKeysSeparate(t *testing.T) {
	keys := map[string][]byte{"a": []byte("0123456789abcdef"), "b": []byte("fedcba9876543210")}
	plain := []byte("media bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keys[r.Header.Get("Cookie")]
		switch r.URL.Path {
		case "/list.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\",IV=0x00000000000000000000000000000000\npart.ts\npart.ts\n#EXT-X-ENDLIST\n")
		case "/key":
			w.Write(key)
		default:
			padding := aes.BlockSize - len(plain)%aes.BlockSize
			data := append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(padding)}, padding)...)
			block, _ := aes.NewCipher(key)
			cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(data, data)
			w.Write(data)
		}
	}))
	defer srv.Close()
	cfg := remediationConfig(t, srv.URL)
	firstSegment, release := make(chan struct{}), make(chan struct{})
	var callbacks atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg.OnProgress = func(_, _, _ int, _, _ int64) {
		if callbacks.Add(1) == 1 {
			close(firstSegment)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	d := NewDownloader(cfg)
	first := make(chan *DownloadResult, 1)
	go func() { first <- d.Download(ctx, srv.URL+"/list.m3u8", "", "a.ts", &DownloadOptions{Cookie: "a"}) }()
	select {
	case <-firstSegment:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A remains live with its first key cached while B uses the same URI under
	// different credentials. With must share output locks, but never key state.
	second := d.With(Overlay{}).Download(ctx, srv.URL+"/list.m3u8", "", "b.ts", &DownloadOptions{Cookie: "b"})
	close(release)
	for _, result := range []*DownloadResult{<-first, second} {
		if result.Error != nil {
			t.Fatal(result.Error)
		}
		b, err := os.ReadFile(filepath.Join(cfg.OutputDir, result.OutputFile))
		if err != nil || !bytes.Equal(b, bytes.Repeat(plain, 2)) {
			t.Fatalf("credential-specific decryption: %q, %v", b, err)
		}
	}
}
