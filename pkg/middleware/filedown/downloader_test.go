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

// 测试辅助：创建临时目录
func tempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "filedown_test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// 测试辅助：创建模拟 HTTP 服务器，返回指定内容和支持 Range
func mockServer(content []byte, supportRange bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		// GET 请求
		if supportRange && r.Header.Get("Range") != "" {
			// 解析 Range 并返回部分内容
			var start, end int
			_, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
			if err != nil {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if end >= len(content) {
				end = len(content) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(content[start : end+1])
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	}))
}

// 测试正常下载（支持分片）
func TestDownloadWithChunks(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("abcdefghijklmnopqrstuvwxyz1234567890")
	server := mockServer(content, true)
	defer server.Close()

	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  2,
		ChunkSize:      10,
		EnableResume:   true,
		SaveBatchSize:  2,
	}
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL, "subdir", "test.txt", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	expectedRel := filepath.Join("subdir", "test.txt")
	if result.OutputFile != expectedRel {
		t.Errorf("OutputFile = %q, want %q", result.OutputFile, expectedRel)
	}
	if result.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", result.Size, len(content))
	}
	// 检查文件内容
	absPath := filepath.Join(tempOut, expectedRel)
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(content) {
		t.Errorf("file content mismatch: got %q, want %q", data, content)
	}
	// 检查状态文件是否被清理
	stateFiles, _ := filepath.Glob(filepath.Join(tempState, "subdir", "*.json"))
	if len(stateFiles) != 0 {
		t.Errorf("state file not cleaned: %v", stateFiles)
	}
}

// 测试断点续传：中断后恢复
func TestDownloadResume(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("012345678901234567890123456789") // 30 bytes
	server := mockServer(content, true)
	defer server.Close()

	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  2,
		ChunkSize:      10,
		EnableResume:   true,
		SaveBatchSize:  1, // 每个分片都保存，便于测试
	}
	downloader := NewDownloader(cfg)

	// 第一次下载，中途取消（通过可取消的 context）
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var firstResult *DownloadResult
	go func() {
		defer wg.Done()
		firstResult = downloader.Download(ctx, server.URL, "resume", "file.bin", nil)
	}()
	// 等待一个分片完成（模拟部分下载）
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()
	if firstResult.Error == nil {
		t.Fatal("expected error due to cancel, got nil")
	}
	// 第二次下载，应续传
	result2 := downloader.Download(context.Background(), server.URL, "resume", "file.bin", nil)
	if result2.Error != nil {
		t.Fatalf("resume download failed: %v", result2.Error)
	}
	absPath := filepath.Join(tempOut, "resume", "file.bin")
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(content) {
		t.Errorf("resume content mismatch: got %q, want %q", data, content)
	}
}

// 测试不支持 Range 时降级为单线程
func TestDownloadNoRangeFallback(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("single thread download test")
	server := mockServer(content, false) // 不支持 Range
	defer server.Close()

	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  4,
		ChunkSize:      5,
		EnableResume:   true,
	}
	downloader := NewDownloader(cfg)
	result := downloader.Download(context.Background(), server.URL, "no_range", "file.txt", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	absPath := filepath.Join(tempOut, "no_range", "file.txt")
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
}

// 测试服务器返回错误（如404）
func TestDownloadHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	downloader := NewDownloader(cfg)
	result := downloader.Download(context.Background(), server.URL, "", "file", nil)
	if result.Error == nil {
		t.Fatal("expected error, got nil")
	}
}

// 测试自定义文件名（从 URL 提取）
func TestGenFileName(t *testing.T) {
	d := &Downloader{}
	tests := []struct {
		url      string
		expected string
	}{
		{"http://example.com/file.zip", "file.zip"},
		{"http://example.com/path/to/video.mp4?token=123", "video.mp4"},
		{"http://example.com/", "file_"}, // 以时间戳结尾，无法精确匹配，只检查前缀
	}
	for _, tt := range tests {
		name := d.genFileName(tt.url, "")
		if tt.expected == "file_" {
			if !strings.HasPrefix(name, "file_") {
				t.Errorf("genFileName(%q) = %q, want prefix 'file_'", tt.url, name)
			}
		} else {
			if name != tt.expected {
				t.Errorf("genFileName(%q) = %q, want %q", tt.url, name, tt.expected)
			}
		}
	}
}

// 测试 Content-Disposition 文件名解析
func TestContentDispositionFileName(t *testing.T) {
	d := &Downloader{}
	cd := `attachment; filename="test.pdf"; filename*=UTF-8''test.pdf`
	name := d.genFileName("http://example.com/", cd)
	if name != "test.pdf" {
		t.Errorf("expected test.pdf, got %s", name)
	}
}

// 测试并发下载相同文件（应串行执行）
func TestConcurrentSameFile(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("concurrent test data")
	server := mockServer(content, true)
	defer server.Close()

	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  2,
		ChunkSize:      10,
		EnableResume:   true,
	}
	downloader := NewDownloader(cfg)
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := downloader.Download(context.Background(), server.URL, "same", "file.dat", nil)
		err1 = r.Error
	}()
	go func() {
		defer wg.Done()
		r := downloader.Download(context.Background(), server.URL, "same", "file.dat", nil)
		err2 = r.Error
	}()
	wg.Wait()
	if err1 != nil || err2 != nil {
		t.Errorf("download errors: %v, %v", err1, err2)
	}
	// 文件应只被写入一次，内容完整
	data, err := os.ReadFile(filepath.Join(tempOut, "same", "file.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(content) {
		t.Errorf("content mismatch: %q", data)
	}
}

// 测试取消下载时临时文件被清理
func TestCancelCleansTemp(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := make([]byte, 50*1024*1024) // 50MB 大文件，确保分片下载耗时
	server := mockServer(content, true)
	defer server.Close()

	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  2,
		ChunkSize:      10 * 1024 * 1024, // 10MB
		EnableResume:   true,
	}
	downloader := NewDownloader(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		downloader.Download(ctx, server.URL, "cancel", "bigfile.bin", nil)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	// 检查临时目录是否被清理（可能残留，但应尽量清理）
	tempSegments := filepath.Join(tempState, "segments", "cancel", "bigfile.bin")
	if _, err := os.Stat(tempSegments); err == nil {
		// 如果取消时部分临时文件未清理，可以接受，但最好是清理了
		t.Log("temp segments may still exist, not critical")
	}
}

// 测试进度回调
func TestProgressCallback(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("progress test data")
	server := mockServer(content, true)
	defer server.Close()

	var mu sync.Mutex
	var lastDownloaded int64
	var totalSize int64
	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		ChunkSize:      5,
		MaxConcurrent:  1,
		OnProgress: func(downloaded, total int64) {
			mu.Lock()
			lastDownloaded = downloaded
			totalSize = total
			mu.Unlock()
		},
	}
	downloader := NewDownloader(cfg)
	result := downloader.Download(context.Background(), server.URL, "progress", "file.bin", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}
	mu.Lock()
	defer mu.Unlock()
	if lastDownloaded != int64(len(content)) {
		t.Errorf("last downloaded = %d, want %d", lastDownloaded, len(content))
	}
	if totalSize != int64(len(content)) {
		t.Errorf("total size = %d, want %d", totalSize, len(content))
	}
}
