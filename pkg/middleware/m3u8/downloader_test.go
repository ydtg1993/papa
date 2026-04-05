package m3u8

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 辅助函数：生成测试用的 M3U8 内容
func generateTestM3U8(segments int, baseURL string) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")
	sb.WriteString("#EXT-X-TARGETDURATION:10\n")
	for i := 0; i < segments; i++ {
		sb.WriteString(fmt.Sprintf("#EXTINF:10.0,\n"))
		sb.WriteString(fmt.Sprintf("segment_%d.ts\n", i))
	}
	return sb.String()
}

// 测试 NewDownloader 和默认配置
func TestNewDownloader(t *testing.T) {
	d := NewDownloader(nil)
	if d.config.OutputDir != "./downloads" {
		t.Errorf("expected OutputDir ./downloads, got %s", d.config.OutputDir)
	}
	if d.config.MaxConcurrent != 5 {
		t.Errorf("expected MaxConcurrent 5, got %d", d.config.MaxConcurrent)
	}
	if d.client.Timeout != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", d.client.Timeout)
	}
}

// 测试 DownloadOptions 传递（通过 mock server 验证请求头）
func TestDownloadOptionsHeaders(t *testing.T) {
	var receivedUserAgent, receivedReferer, receivedCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUserAgent = r.Header.Get("User-Agent")
		receivedReferer = r.Header.Get("Referer")
		receivedCookie = r.Header.Get("Cookie")
		w.Write([]byte(generateTestM3U8(2, "")))
	}))
	defer server.Close()

	downloader := NewDownloader(DefaultConfig())
	opts := &DownloadOptions{
		UserAgent: "TestAgent/1.0",
		Referer:   "https://test.com",
		Cookie:    "session=abc",
	}
	ctx := context.Background()
	result := downloader.Download(ctx, server.URL, "", opts)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	if receivedUserAgent != "TestAgent/1.0" {
		t.Errorf("User-Agent not set correctly, got %s", receivedUserAgent)
	}
	if receivedReferer != "https://test.com" {
		t.Errorf("Referer not set correctly, got %s", receivedReferer)
	}
	if receivedCookie != "session=abc" {
		t.Errorf("Cookie not set correctly, got %s", receivedCookie)
	}
}

// 测试解析 M3U8 并下载片段（使用 mock 片段服务器）
func TestDownloadSegments(t *testing.T) {
	// 创建片段服务器
	segmentContent := []byte("dummy ts content")
	segmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(segmentContent)
	}))
	defer segmentServer.Close()

	// 创建 M3U8 服务器，返回引用片段服务器的 M3U8
	m3u8Content := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXTINF:10.0,
` + segmentServer.URL + `/segment_0.ts
#EXTINF:10.0,
` + segmentServer.URL + `/segment_1.ts
`
	m3u8Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	}))
	defer m3u8Server.Close()

	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.ResumeStateDir = filepath.Join(tmpDir, ".resume")
	cfg.AutoMerge = false // 直接拼接 TS 文件，避免依赖 ffmpeg
	cfg.KeepSegmentsAfterMerge = true

	downloader := NewDownloader(cfg)
	ctx := context.Background()
	outputFile := "test_output.ts"
	result := downloader.Download(ctx, m3u8Server.URL, outputFile, nil)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	if result.Segments != 2 {
		t.Errorf("expected 2 segments, got %d", result.Segments)
	}
	outputPath := filepath.Join(tmpDir, outputFile)
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		t.Errorf("output file not created")
	}
	// 检查文件大小：两个片段内容 + 无 init 段
	expectedSize := int64(len(segmentContent) * 2)
	if result.Size != expectedSize {
		t.Errorf("expected total size %d, got %d", expectedSize, result.Size)
	}
	info, _ := os.Stat(outputPath)
	if info.Size() != expectedSize {
		t.Errorf("output file size mismatch, expected %d, got %d", expectedSize, info.Size())
	}
}

// 测试限速器
func TestRateLimiter(t *testing.T) {
	limiter := NewRateLimiter(1) // 1 KB/s
	if limiter == nil {
		t.Fatal("limiter should not be nil")
	}
	ctx := context.Background()
	start := time.Now()
	// 请求 2KB，应该至少花费 2 秒
	err := limiter.Wait(ctx, 2048)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed < 2*time.Second {
		t.Errorf("rate limiter too fast: %v, expected >=2s", elapsed)
	}
}

// 测试断点续传
func TestResume(t *testing.T) {
	segmentContent := []byte("test data")
	var requestCount int32
	segmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.Write(segmentContent)
	}))
	defer segmentServer.Close()

	m3u8Content := `#EXTM3U
#EXTINF:10.0,
` + segmentServer.URL + `/seg0.ts
#EXTINF:10.0,
` + segmentServer.URL + `/seg1.ts
`
	m3u8Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	}))
	defer m3u8Server.Close()

	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.ResumeStateDir = filepath.Join(tmpDir, ".resume")
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1 // 顺序下载，便于模拟部分完成
	cfg.EnableResume = true

	downloader := NewDownloader(cfg)

	// 第一次下载，在第一个片段完成后故意中断（模拟）
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		result := downloader.Download(ctx, m3u8Server.URL, "resume_test.ts", nil)
		if result.Error == nil || !strings.Contains(result.Error.Error(), "context canceled") {
			t.Errorf("expected context canceled error, got %v", result.Error)
		}
	}()
	// 等待第一个片段下载完成（通过检查临时文件）
	time.Sleep(500 * time.Millisecond)
	cancel()
	wg.Wait()

	// 第二次下载，应该只下载第二个片段
	atomic.StoreInt32(&requestCount, 0)
	result2 := downloader.Download(context.Background(), m3u8Server.URL, "resume_test.ts", nil)
	if result2.Error != nil {
		t.Fatalf("resume download failed: %v", result2.Error)
	}
	// 第二个片段应该只被请求一次（第一次下载时第一个片段已保存）
	if atomic.LoadInt32(&requestCount) != 1 {
		t.Errorf("expected 1 request for segment, got %d", atomic.LoadInt32(&requestCount))
	}
}

// 测试密钥缓存
func TestKeyCache(t *testing.T) {
	var keyRequestCount int32
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&keyRequestCount, 1)
		// 模拟 16 字节 AES 密钥
		w.Write([]byte("1234567890123456"))
	}))
	defer keyServer.Close()

	segmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟加密数据（简单填充，实际解密测试较复杂，这里仅验证缓存逻辑）
		w.Write([]byte("encrypted data"))
	}))
	defer segmentServer.Close()

	m3u8Content := `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="` + keyServer.URL + `",IV=0x1234567890abcdef1234567890abcdef
#EXTINF:10.0,
` + segmentServer.URL + `/seg0.ts
#EXTINF:10.0,
` + segmentServer.URL + `/seg1.ts
`
	m3u8Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	}))
	defer m3u8Server.Close()

	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.ResumeStateDir = filepath.Join(tmpDir, ".resume")
	cfg.AutoMerge = false
	// 注意：实际解密会失败因为数据不是合法 AES 加密，但我们的测试只关心 prepareKey 被调用的次数
	// 为了测试缓存，我们需要绕过解密错误，或者允许失败但检查 keyRequestCount
	downloader := NewDownloader(cfg)
	ctx := context.Background()
	_ = downloader.Download(ctx, m3u8Server.URL, "keycache.ts", nil)
	// 由于解密会失败，download 可能报错，但 prepareKey 仍然会被调用两次（每个片段）
	// 检查 key 请求次数应为 1（缓存生效）
	if atomic.LoadInt32(&keyRequestCount) != 1 {
		t.Errorf("expected key requested once, got %d", atomic.LoadInt32(&keyRequestCount))
	}
}

// 测试 concatTSFiles 流式拼接
func TestConcatTSFiles(t *testing.T) {
	tmpDir := t.TempDir()
	initPath := filepath.Join(tmpDir, "init.ts")
	seg1 := filepath.Join(tmpDir, "seg1.ts")
	seg2 := filepath.Join(tmpDir, "seg2.ts")
	output := filepath.Join(tmpDir, "out.ts")

	initData := []byte("INIT")
	segData1 := []byte("SEG1")
	segData2 := []byte("SEG2")
	os.WriteFile(initPath, initData, 0644)
	os.WriteFile(seg1, segData1, 0644)
	os.WriteFile(seg2, segData2, 0644)

	downloader := &Downloader{config: &Config{}}
	err := downloader.concatTSFiles(initPath, []string{seg1, seg2}, output)
	if err != nil {
		t.Fatalf("concatTSFiles failed: %v", err)
	}
	data, _ := os.ReadFile(output)
	expected := append(initData, append(segData1, segData2...)...)
	if string(data) != string(expected) {
		t.Errorf("concat result incorrect: got %s, expected %s", data, expected)
	}
}

// 测试断点续传状态文件读写
func TestResumeStatePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.ResumeStateDir = tmpDir
	downloader := NewDownloader(cfg)

	state := &ResumeState{
		M3U8URL:       "http://example.com/playlist.m3u8",
		OutputFile:    "video.ts",
		TotalSegments: 10,
		Completed:     []int{0, 1, 2},
		SegmentFiles:  []string{"a.ts", "b.ts", "c.ts"},
	}
	err := downloader.saveResumeState(state)
	if err != nil {
		t.Fatalf("saveResumeState failed: %v", err)
	}

	loaded, err := downloader.loadResumeState(state.M3U8URL, state.OutputFile)
	if err != nil {
		t.Fatalf("loadResumeState failed: %v", err)
	}
	if loaded.TotalSegments != state.TotalSegments {
		t.Errorf("TotalSegments mismatch")
	}
	if len(loaded.Completed) != len(state.Completed) {
		t.Errorf("Completed length mismatch")
	}
}

// 测试多码率选择
func TestSelectBestStream(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000
medium.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000
high.m3u8
`
	baseURL := "http://example.com/"
	downloader := &Downloader{}
	bestURL, err := downloader.selectBestStream(playlist, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	expected := "http://example.com/high.m3u8"
	if bestURL != expected {
		t.Errorf("expected %s, got %s", expected, bestURL)
	}
}

// 测试 parsePlaylistEnhancedWithIndex 解析带 BYTERANGE 的 M3U8
func TestParsePlaylistWithByteRange(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-MAP:URI="init.ts"
#EXT-X-KEY:METHOD=AES-128,URI="key.bin"
#EXT-X-BYTERANGE:1024@0
segment.ts
#EXTINF:10.0,
segment2.ts
`
	baseURL := "http://test.com/"
	downloader := &Downloader{}
	initSeg, segs, keys, err := downloader.parsePlaylistEnhancedWithIndex(playlist, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if initSeg == nil || initSeg.URL != "http://test.com/init.ts" {
		t.Errorf("init segment not parsed correctly")
	}
	if len(segs) != 2 {
		t.Errorf("expected 2 segments, got %d", len(segs))
	}
	if segs[0].Range != "1024@0" {
		t.Errorf("byte range not parsed: %s", segs[0].Range)
	}
	if keys[0] == nil || keys[0].URL != "http://test.com/key.bin" {
		t.Errorf("key not associated correctly")
	}
}

// 测试 Download 可变参数 opts
func TestDownloadVariadicOpts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(generateTestM3U8(1, "")))
	}))
	defer server.Close()

	downloader := NewDownloader(DefaultConfig())
	// 不传 opts
	result1 := downloader.Download(context.Background(), server.URL, "", nil)
	if result1.Error != nil {
		t.Fatalf("download without opts failed: %v", result1.Error)
	}
	// 传 nil opts
	result2 := downloader.Download(context.Background(), server.URL, "", (*DownloadOptions)(nil))
	if result2.Error != nil {
		t.Fatalf("download with nil opts failed: %v", result2.Error)
	}
	// 传有效 opts
	opts := &DownloadOptions{UserAgent: "test"}
	result3 := downloader.Download(context.Background(), server.URL, "", opts)
	if result3.Error != nil {
		t.Fatalf("download with opts failed: %v", result3.Error)
	}
}
