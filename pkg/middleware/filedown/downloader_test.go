package filedown

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 生成测试数据（确定性的字节序列）
func generateTestData(size int64) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	return data
}

// 创建测试 HTTP 服务器，支持 Range 请求
func newTestServer(data []byte, supportRange bool) *httptest.Server {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HEAD 请求
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			if supportRange {
				w.Header().Set("Accept-Ranges", "bytes")
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		// GET 请求
		if r.Method == http.MethodGet {
			rangeHeader := r.Header.Get("Range")
			if rangeHeader != "" && supportRange {
				var start, end int64
				_, _ = fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
				if end >= int64(len(data)) {
					end = int64(len(data)) - 1
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start : end+1])
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	return httptest.NewServer(handler)
}

// TestDownload_SuccessWithChunks 测试支持分片的完整下载
func TestDownload_SuccessWithChunks(t *testing.T) {
	data := generateTestData(10 * 1024 * 1024) // 10MB
	server := newTestServer(data, true)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ChunkSize = 2 * 1024 * 1024 // 2MB 分片
	cfg.MaxConcurrent = 3

	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{
		UserAgent: "TestAgent/1.0",
		Headers:   map[string]string{"X-Custom": "hello"},
	}
	ctx := context.Background()
	result := downloader.Download(ctx, server.URL, "output.bin", opts)

	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	if result.Size != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), result.Size)
	}
	// 校验文件内容
	got, err := os.ReadFile(result.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("file length mismatch: %d vs %d", len(got), len(data))
	}
	for i := range data {
		if got[i] != data[i] {
			t.Errorf("data mismatch at offset %d", i)
			break
		}
	}
}

// TestDownload_FallbackToSingle 测试不支持 Range 时降级为单线程
func TestDownload_FallbackToSingle(t *testing.T) {
	data := generateTestData(1 * 1024 * 1024)
	server := newTestServer(data, false) // 不支持 Range
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{UserAgent: "test"}
	result := downloader.Download(context.Background(), server.URL, "", opts)

	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	if result.Size != int64(len(data)) {
		t.Errorf("size mismatch: %d vs %d", result.Size, len(data))
	}
}

// TestDownload_HTTPError 测试 HTTP 错误（如 404）
func TestDownload_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{UserAgent: "test"}
	result := downloader.Download(context.Background(), server.URL, "", opts)

	if result.Error == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestDownload_Timeout 测试超时场景
func TestDownload_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.Timeout = 500 * time.Millisecond
	cfg.OutputDir = t.TempDir()
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{UserAgent: "test"}
	result := downloader.Download(context.Background(), server.URL, "", opts)

	if result.Error == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// TestDownload_CustomHeaders 测试自定义 Headers 和 Cookie
func TestDownload_CustomHeaders(t *testing.T) {
	var mu sync.Mutex
	received := struct {
		UserAgent string
		Referer   string
		Cookie    string
		XCustom   string
	}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		received.UserAgent = r.Header.Get("User-Agent")
		received.Referer = r.Header.Get("Referer")
		received.Cookie = r.Header.Get("Cookie")
		received.XCustom = r.Header.Get("X-Custom")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{
		UserAgent: "MyUA/1.0",
		Referer:   "https://example.com",
		Cookie:    "session=abc123",
		Headers:   map[string]string{"X-Custom": "hello"},
	}
	_ = downloader.Download(context.Background(), server.URL, "", opts)

	time.Sleep(100 * time.Millisecond) // 等待请求处理
	mu.Lock()
	defer mu.Unlock()
	if got := received.UserAgent; got != "MyUA/1.0" {
		t.Errorf("User-Agent = %q, want %q", got, "MyUA/1.0")
	}
	if got := received.Referer; got != "https://example.com" {
		t.Errorf("Referer = %q, want %q", got, "https://example.com")
	}
	if got := received.Cookie; got != "session=abc123" {
		t.Errorf("Cookie = %q, want %q", got, "session=abc123")
	}
	if got := received.XCustom; got != "hello" {
		t.Errorf("X-Custom = %q, want %q", got, "hello")
	}
}

// TestDownload_Concurrent 测试并发下载多个文件（同一个 Downloader 实例）
func TestDownload_Concurrent(t *testing.T) {
	data := generateTestData(2 * 1024 * 1024)
	server := newTestServer(data, true)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.MaxConcurrent = 4
	downloader := NewDownloader(cfg)

	const jobs = 5
	var wg sync.WaitGroup
	errs := make(chan error, jobs)

	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			opts := &DownloadOptions{UserAgent: fmt.Sprintf("UA-%d", idx)}
			result := downloader.Download(context.Background(), server.URL, fmt.Sprintf("file%d.bin", idx), opts)
			if result.Error != nil {
				errs <- fmt.Errorf("job %d: %v", idx, result.Error)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// TestDownload_ProgressCallback 测试进度回调（分片模式）
func TestDownload_ProgressCallback(t *testing.T) {
	data := generateTestData(6 * 1024 * 1024) // 6MB
	server := newTestServer(data, true)
	defer server.Close()

	var mu sync.Mutex
	var lastDownloaded, lastTotal int64
	progressCalled := false

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ChunkSize = 2 * 1024 * 1024 // 3 个分片
	cfg.OnProgress = func(downloaded, total int64) {
		mu.Lock()
		defer mu.Unlock()
		lastDownloaded = downloaded
		lastTotal = total
		progressCalled = true
	}
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{UserAgent: "test"}
	result := downloader.Download(context.Background(), server.URL, "", opts)

	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	if !progressCalled {
		t.Error("progress callback never called")
	}
	mu.Lock()
	defer mu.Unlock()
	if lastTotal != int64(len(data)) {
		t.Errorf("final total = %d, want %d", lastTotal, len(data))
	}
	// 最终下载量应该等于总大小（可能由于实时累加会超过，但最后回调应该正好是总大小）
	if lastDownloaded != int64(len(data)) {
		t.Errorf("final downloaded = %d, want %d", lastDownloaded, len(data))
	}
}

// TestDownload_ContextCancel 测试主动取消下载
func TestDownload_ContextCancel(t *testing.T) {
	data := generateTestData(10 * 1024 * 1024)
	server := newTestServer(data, true)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.ChunkSize = 2 * 1024 * 1024
	downloader := NewDownloader(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	opts := &DownloadOptions{UserAgent: "test"}

	// 100ms 后取消
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	result := downloader.Download(ctx, server.URL, "", opts)
	if result.Error == nil {
		t.Fatal("expected cancel error, got nil")
	}
}

// TestDownload_FilenameFromContentDisposition 测试从 Content-Disposition 提取文件名
func TestDownload_FilenameFromContentDisposition(t *testing.T) {
	data := []byte("test content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Disposition", `attachment; filename="myfile.txt"`)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{UserAgent: "test"}
	result := downloader.Download(context.Background(), server.URL, "", opts)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	expected := "myfile.txt"
	if filepath.Base(result.OutputFile) != expected {
		t.Errorf("expected filename %q, got %q", expected, filepath.Base(result.OutputFile))
	}
}

// TestDownload_RetryOnFailure 测试分片失败重试
func TestDownload_RetryOnFailure(t *testing.T) {
	data := generateTestData(2 * 1024 * 1024)
	var failFirst bool
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 针对第一个分片（bytes=0-...）故意失败一次
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Range"), "bytes=0-") {
			mu.Lock()
			if !failFirst {
				failFirst = true
				mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Unlock()
		}
		// 正常处理
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodGet {
			var start, end int64
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
		}
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	cfg.MaxRetries = 2
	cfg.RetryInterval = 0 // 立即重试
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{UserAgent: "test"}
	result := downloader.Download(context.Background(), server.URL, "", opts)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	// 验证文件内容
	got, err := os.ReadFile(result.OutputFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) {
		t.Fatalf("size mismatch: %d vs %d", len(got), len(data))
	}
}

// TestDownload_EmptyUserAgent 测试未设置 UserAgent 时不覆盖默认 UA
func TestDownload_EmptyUserAgent(t *testing.T) {
	var receivedUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	downloader := NewDownloader(cfg)
	opts := &DownloadOptions{
		// UserAgent 留空
		UserAgent: "",
	}
	_ = downloader.Download(context.Background(), server.URL, "", opts)

	time.Sleep(100 * time.Millisecond)
	// 期望 Go 默认的 User-Agent（不是空字符串）
	if receivedUA == "" {
		t.Error("User-Agent should not be empty when not set")
	}
	// 可选：检查是否包含 "Go-http-client"
	if !strings.Contains(receivedUA, "Go-http-client") {
		t.Logf("User-Agent = %q, expected default Go UA", receivedUA)
	}
}
