package m3u8

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 状态文件里 SegmentFiles 被改短（段数对得上、数组却短一截）时，不能越界 panic，而是当作状态损坏重来。
//
// 回归点：老代码只校验 TotalSegments，随后 `resumeState.SegmentFiles[index] = …` 直接按下标写 ——
// 数组短了就是越界 panic。
func TestResumeShortSegmentFilesRestarts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1.0,\nseg0.ts\n#EXTINF:1.0,\nseg1.ts\n#EXT-X-ENDLIST\n")
	})
	for _, name := range []string{"seg0.ts", "seg1.ts"} {
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", 32))
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.AutoMerge = false
	dl := NewDownloader(cfg)

	const outDir, outFile = "sub", "movie"
	url := server.URL + "/playlist.m3u8"
	la := &labor{source: url, outDir: outDir, filename: outFile}
	stale := &ResumeState{
		M3U8URL: url, OutputFile: outFile, TotalSegments: 2,
		Completed:    []int{0},
		SegmentFiles: []string{"only-one"}, // 数组比段数短
	}
	if err := dl.saveResumeState(la, stale); err != nil {
		t.Fatal(err)
	}

	mode := OutputSegments
	res := dl.Download(context.Background(), url, outDir, outFile, &DownloadOptions{Output: &mode})
	if res.Error != nil {
		t.Fatalf("短 SegmentFiles 应当当作状态损坏重来，而不是 panic / 报错：%v", res.Error)
	}
	if res.Segments != 2 {
		t.Fatalf("应当重新下 2 段，实得 %d", res.Segments)
	}
}

// OutputSegments 模式下，EXT-X-MAP 的 init 段也要进产物目录。
//
// 回归点：老代码只挪媒体分片，init 段漏在临时目录里 —— fMP4 缺 init 段没法解码，错误要等播放器
// 解不出来才暴露，而且 init 临时文件还留着。
func TestOutputSegmentsIncludesInitSegment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:1.0,\nseg0.m4s\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/init.mp4", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "INITDATA") })
	mux.HandleFunc("/seg0.m4s", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, strings.Repeat("x", 16)) })
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxConcurrent = 1
	cfg.EnableResume = false
	dl := NewDownloader(cfg)

	const outDir, outFile = "sub", "movie"
	mode := OutputSegments
	res := dl.Download(context.Background(), server.URL+"/playlist.m3u8", outDir, outFile, &DownloadOptions{Output: &mode})
	if res.Error != nil {
		t.Fatalf("Download: %v", res.Error)
	}

	entries, err := os.ReadDir(filepath.Join(cfg.OutputDir, outDir, outFile))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["init.ts"] {
		t.Fatalf("OutputSegments 产物里缺 init 段（EXT-X-MAP）：%v", names)
	}
	if !names["segment_00000.ts"] {
		t.Fatalf("缺媒体分片：%v", names)
	}
}

// EXT-X-MAP 的 BYTERANGE 要解析成合法的 HTTP Range（媒体段的 BYTERANGE 早已支持，init 段漏了）。
func TestParseMapTagByteRange(t *testing.T) {
	d := NewDownloader(DefaultConfig())
	seg := d.parseMapTag(`#EXT-X-MAP:URI="init.mp4",BYTERANGE="1024@2048"`, "http://x/")
	if seg == nil {
		t.Fatal("parseMapTag 应返回 init 段")
	}
	if seg.Range != "2048-3071" {
		t.Fatalf("BYTERANGE 应解析成 2048-3071，实得 %q", seg.Range)
	}

	if seg2 := d.parseMapTag(`#EXT-X-MAP:URI="init.mp4"`, "http://x/"); seg2 == nil || seg2.Range != "" {
		t.Fatalf("没有 BYTERANGE 时 Range 应为空，实得 %v", seg2)
	}
}
