package m3u8

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 测试辅助：创建临时目录并自动清理
func tempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "m3u8_test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// 测试 generateFileName
func TestGenerateFileName(t *testing.T) {
	d := &Downloader{}
	tests := []struct {
		url      string
		expected string
	}{
		{"https://example.com/video.m3u8", "video.ts"},
		{"https://example.com/path/playlist.m3u8", "playlist.ts"},
		{"https://example.com/video", "video.ts"},
		{"https://example.com/", "video_.ts"}, // 会包含时间戳，仅检查前缀
	}
	for _, tt := range tests {
		name := d.generateFileName(tt.url)
		if tt.expected == "video_.ts" {
			if !strings.HasPrefix(name, "video_") || !strings.HasSuffix(name, ".ts") {
				t.Errorf("generateFileName(%q) = %q, want prefix 'video_' and suffix '.ts'", tt.url, name)
			}
		} else {
			if name != tt.expected {
				t.Errorf("generateFileName(%q) = %q, want %q", tt.url, name, tt.expected)
			}
		}
	}
}

// 测试 resolveURL
func TestResolveURL(t *testing.T) {
	d := &Downloader{}
	tests := []struct {
		raw, base, expected string
	}{
		{"segment.ts", "https://example.com/path/", "https://example.com/path/segment.ts"},
		{"/segment.ts", "https://example.com/path/", "https://example.com/segment.ts"},
		{"https://other.com/seg.ts", "https://example.com/", "https://other.com/seg.ts"},
		{"segment.ts", "", "segment.ts"},
	}
	for _, tt := range tests {
		got := d.resolveURL(tt.raw, tt.base)
		if got != tt.expected {
			t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.raw, tt.base, got, tt.expected)
		}
	}
}

// 测试解析播放列表（带索引）
func TestParsePlaylistEnhancedWithIndex(t *testing.T) {
	d := &Downloader{}
	playlist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MAP:URI="init.ts"
#EXTINF:5,
segment1.ts
#EXTINF:5,
segment2.ts
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1234567890abcdef1234567890abcdef
#EXTINF:5,
segment3.ts
#EXT-X-BYTERANGE:1024@0
segment4.ts
`
	baseURL := "https://example.com/path/"
	init, segs, keys, err := d.parsePlaylistEnhancedWithIndex(playlist, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if init == nil || init.URL != "https://example.com/path/init.ts" {
		t.Errorf("init segment wrong: %v", init)
	}
	if len(segs) != 4 {
		t.Fatalf("expected 4 segments, got %d", len(segs))
	}
	// 检查 segment1
	if segs[0].URL != "https://example.com/path/segment1.ts" || segs[0].Index != 0 {
		t.Errorf("segment1: %+v", segs[0])
	}
	// 检查 segment2
	if segs[1].URL != "https://example.com/path/segment2.ts" || segs[1].Index != 1 {
		t.Errorf("segment2: %+v", segs[1])
	}
	// 检查 segment3 带密钥
	if keys[2] == nil || keys[2].URL != "https://example.com/path/key.bin" || keys[2].IV != "0x1234567890abcdef1234567890abcdef" {
		t.Errorf("segment3 key: %v", keys[2])
	}
	// 检查 byte range
	if segs[3].Range != "1024@0" {
		t.Errorf("segment4 range: %s", segs[3].Range)
	}
}

// 测试多码率流选择
func TestSelectBestStream(t *testing.T) {
	d := &Downloader{}
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1280000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000
mid.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5120000
high.m3u8
`
	baseURL := "https://example.com/"
	best, err := d.selectBestStream(playlist, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if best != "https://example.com/high.m3u8" {
		t.Errorf("best stream = %q, want high.m3u8", best)
	}
}

// 测试完整下载流程（不依赖 ffmpeg，使用 concatTSFiles）
func TestDownloadIntegration(t *testing.T) {
	// 创建模拟 HTTP 服务器
	mux := http.NewServeMux()
	// 主播放列表
	m3u8Content := `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:1.0,
segment1.ts
#EXTINF:1.0,
segment2.ts
`
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(m3u8Content))
	})
	// 片段内容
	seg1 := []byte("SEGMENT1 DATA")
	seg2 := []byte("SEGMENT2 DATA")
	mux.HandleFunc("/segment1.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(seg1)
	})
	mux.HandleFunc("/segment2.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(seg2)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// 创建临时目录
	outDir := tempDir(t)
	stateDir := tempDir(t)

	cfg := DefaultConfig()
	cfg.OutputDir = outDir
	cfg.ResumeStateDir = stateDir
	cfg.AutoMerge = false // 避免调用 ffmpeg
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.SaveBatchSize = 1
	downloader := NewDownloader(cfg)

	url := server.URL + "/playlist.m3u8"
	outputFile := "test_output.ts"
	outputDirRel := "subdir"

	result := downloader.Download(context.Background(), url, outputDirRel, outputFile, nil)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	if result.Segments != 2 {
		t.Errorf("Segments = %d, want 2", result.Segments)
	}
	expectedRel := filepath.Join(outputDirRel, outputFile)
	if result.OutputFile != expectedRel {
		t.Errorf("OutputFile = %q, want %q", result.OutputFile, expectedRel)
	}
	// 检查输出文件内容
	absPath := filepath.Join(outDir, expectedRel)
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedData := append(seg1, seg2...)
	if string(data) != string(expectedData) {
		t.Errorf("file content mismatch: got %q, want %q", data, expectedData)
	}
	// 检查状态文件是否已清理
	stateFiles, _ := filepath.Glob(filepath.Join(stateDir, outputDirRel, "*.json"))
	if len(stateFiles) != 0 {
		t.Errorf("state file not cleaned: %v", stateFiles)
	}
}

// 测试断点续传：取消下载后恢复
func TestDownloadResume(t *testing.T) {
	// 模拟服务器，每个片段响应稍慢
	mux := http.NewServeMux()
	m3u8Content := `#EXTM3U
#EXTINF:1,
seg1.ts
#EXTINF:1,
seg2.ts
`
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	})
	seg1 := []byte("FIRST")
	seg2 := []byte("SECOND")
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write(seg1)
	})
	mux.HandleFunc("/seg2.ts", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write(seg2)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	outDir := tempDir(t)
	stateDir := tempDir(t)
	cfg := DefaultConfig()
	cfg.OutputDir = outDir
	cfg.ResumeStateDir = stateDir
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 2
	cfg.EnableResume = true
	cfg.SaveBatchSize = 1
	downloader := NewDownloader(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		res := downloader.Download(ctx, server.URL+"/playlist.m3u8", "resume", "video.ts", nil)
		firstErr = res.Error
	}()
	// 等待第一个片段完成，然后取消
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if firstErr == nil {
		t.Fatal("expected error due to cancel, got nil")
	}
	// 第二次下载应续传
	res2 := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "resume", "video.ts", nil)
	if res2.Error != nil {
		t.Fatalf("resume download failed: %v", res2.Error)
	}
	absPath := filepath.Join(outDir, "resume", "video.ts")
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := append(seg1, seg2...)
	if string(data) != string(expected) {
		t.Errorf("resume content mismatch: got %q, want %q", data, expected)
	}
}

// 测试并发下载相同任务（应串行执行）
func TestConcurrentSameTask(t *testing.T) {
	mux := http.NewServeMux()
	m3u8Content := `#EXTM3U
#EXTINF:1,
seg.ts
`
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	})
	seg := []byte("DATA")
	mux.HandleFunc("/seg.ts", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write(seg)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	outDir := tempDir(t)
	stateDir := tempDir(t)
	cfg := DefaultConfig()
	cfg.OutputDir = outDir
	cfg.ResumeStateDir = stateDir
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1
	downloader := NewDownloader(cfg)

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		res := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "concurrent", "file.ts", nil)
		err1 = res.Error
	}()
	go func() {
		defer wg.Done()
		res := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "concurrent", "file.ts", nil)
		err2 = res.Error
	}()
	wg.Wait()
	if err1 != nil || err2 != nil {
		t.Errorf("errors: %v, %v", err1, err2)
	}
}
