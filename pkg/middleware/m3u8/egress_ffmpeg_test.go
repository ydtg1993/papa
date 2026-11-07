package m3u8

import (
	"context"
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

func TestEgressFFmpegMergeOnlyReadsLocalFiles(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_FFMPEG_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_FFMPEG_BIN not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := DefaultConfig()
	cfg.ResumeStateDir = t.TempDir()
	cfg.FfmpegPath = bin
	d := NewDownloader(cfg)
	part := filepath.Join(t.TempDir(), "part.ts")
	// A real local media fixture must still merge successfully.
	out, err := exec.CommandContext(ctx, bin, "-f", "lavfi", "-i", "sine=frequency=440:duration=0.1", "-c:a", "aac", "-f", "mpegts", part).CombinedOutput()
	if err != nil {
		t.Fatalf("generate local fixture: %v: %s", err, out)
	}
	if err := d.mergeToMP4(ctx, "", []string{part}, filepath.Join(t.TempDir(), "ok.mp4")); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	nested := filepath.Join(t.TempDir(), "nested.m3u8")
	body := fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\n%s/part.ts\n#EXT-X-ENDLIST\n", srv.URL)
	if err := os.WriteFile(nested, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	err = d.mergeToMP4(ctx, "", []string{nested}, filepath.Join(t.TempDir(), "denied.mp4"))
	if err == nil || !strings.Contains(err.Error(), "not on whitelist") {
		t.Fatalf("expected protocol rejection, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("ffmpeg escaped HTTP guard: %d hits", hits.Load())
	}
}
