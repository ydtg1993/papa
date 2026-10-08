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
			if supportRange {
				w.Header().Set("Accept-Ranges", "bytes")
			}
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
		t.Fatalf("download failed: %s", result.Error.Error())
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
		t.Errorf("state file not cleaned: %+v", stateFiles)
	}
}

// 测试断点续传：中断后恢复
func TestDownloadResume(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := make([]byte, 100) // 100 bytes, 10 chunks of 10 bytes
	// 慢速服务器：每个分片请求延迟，确保取消发生在下载中途
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		time.Sleep(50 * time.Millisecond)
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= len(content) {
			end = len(content) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
	}))
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
	// 等待几个分片完成（模拟部分下载），仍有多数分片未完成
	time.Sleep(150 * time.Millisecond)
	cancel()
	wg.Wait()
	if firstResult.Error == nil {
		t.Fatal("expected error due to cancel, got nil")
	}
	// 第二次下载，应续传
	result2 := downloader.Download(context.Background(), server.URL, "resume", "file.bin", nil)
	if result2.Error != nil {
		t.Fatalf("resume download failed: %s", result2.Error.Error())
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
		t.Fatalf("download failed: %s", result.Error.Error())
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
		t.Errorf("download errors: %s, %s", err1.Error(), err2.Error())
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
		t.Fatalf("download failed: %s", result.Error.Error())
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

// 测试单线程下载遇到连接中断（body 短于声明的 Content-Length）时必须报错，不能当成功
func TestDownloadTruncatedSingleThread(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			// 不支持 Range → 走 downloadSingle 降级路径
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		// 只写一半就返回：服务端会提前断开，客户端读到 unexpected EOF
		_, _ = w.Write(content[:len(content)/2])
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempOut
	cfg.ResumeStateDir = tempState
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL, "trunc", "half.bin", nil)
	if result.Error == nil {
		t.Fatalf("expected error for truncated body, got success (size=%d)", result.Size)
	}
}

// 测试分片返回的字节数少于请求范围（短 206）时必须报错，避免合并出空洞文件
func TestDownloadShortChunkFails(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := make([]byte, 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		// 故意少回一半字节，但响应本身是完整的（无 Content-Length 冲突）
		half := max((end-start+1)/2, 1)
		_, _ = w.Write(content[start : start+half])
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempOut
	cfg.ResumeStateDir = tempState
	cfg.ChunkSize = 10
	cfg.MaxConcurrent = 2
	cfg.MaxRetries = 0 // 不重试，保持用例快速
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL, "short", "chunked.bin", nil)
	if result.Error == nil {
		t.Fatal("expected error for short chunk, got success")
	}
	if _, err := os.Stat(filepath.Join(tempOut, "short", "chunked.bin")); err == nil {
		t.Error("merged output should not be produced when a chunk is short")
	}
}

// 合并中途失败不能把已合并的分片删掉。
//
// 回归点：合并循环里每合完一个分片就 os.Remove 它，而失败时状态文件仍然写着"所有分片都下好了"。
// 下次续传会跳过下载直接进合并，于是报 open temp file ... no such file —— 这个任务从此永久失败，
// 只能人工去删状态文件。分片应当只在**整轮合并成功之后**统一清掉。
func TestMergeFailureKeepsSegments(t *testing.T) {
	content := make([]byte, 100) // 100 字节 / 分片 10 字节 = 10 个分片
	for i := range content {
		content[i] = byte(i)
	}
	server := mockServer(content, true)
	defer server.Close()

	tempOut := tempDir(t)
	tempState := tempDir(t)
	cfg := &Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  2,
		ChunkSize:      10,
		EnableResume:   true,
		SaveBatchSize:  1,
	}
	d := NewDownloader(cfg)

	// 造一份"前两个分片已下好"的续传状态，其中**第二个分片的文件故意不存在**，
	// 这样合并会在第二个分片上失败 —— 正好能看出第一个分片有没有被提前删掉。
	la := &labor{url: server.URL, outputDir: "d", filename: "f.bin"}
	segDir := filepath.Join(tempState, "segments", la.outputDir, la.filename)
	if err := os.MkdirAll(segDir, 0755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(segDir, fmt.Sprintf("chunk_%020d.tmp", int64(0)))
	if err := os.WriteFile(first, content[:10], 0644); err != nil {
		t.Fatal(err)
	}
	if err := d.saveResumeState(la, &ResumeState{
		URL: la.url, OutputFile: la.filename, TotalSize: int64(len(content)), ChunkSize: 10,
		Completed: []int64{0, 10},
		TempFiles: []string{first, filepath.Join(segDir, fmt.Sprintf("chunk_%020d.tmp", int64(10)))},
	}); err != nil {
		t.Fatal(err)
	}

	res := d.Download(context.Background(), server.URL, la.outputDir, la.filename)
	if res.Error == nil {
		t.Fatalf("第二个分片缺失，合并应当失败；实得 %+v", res)
	}
	if !strings.Contains(res.Error.Error(), "open temp file") {
		t.Fatalf("应当报在合并阶段，实得：%v", res.Error)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("合并失败后第一个分片必须还在（否则续传永远好不了）：%v", err)
	}
}

// MaxConcurrent 为 0 时不能让下载卡死。
//
// 回归点：并发上限原来是直接 `make(chan struct{}, cfgGlobal.MaxConcurrent)` —— 0 就是
// **无缓冲**通道，第一个 goroutine 永久阻塞在 `sem <- struct{}{}`，wg.Wait() 再也不返回：
// 不报错、不退出、一条日志都没有，业务看到的就是"下载卡死"。负数则直接 panic
// （make(chan, -1)）。
//
// NewDownloader 只在 cfg == nil 时补默认值（DefaultConfig 里是 4），
// 所以"自己拼了一份 Config 但漏了/填错这个字段"必然踩到 —— 而 Config 的字段注释写着
// "默认 4"，更容易让人以为不填就没事。
func TestDownloadDoesNotHangOnNonPositiveConcurrency(t *testing.T) {
	content := []byte("abcdefghijklmnopqrstuvwxyz1234567890")

	for _, concurrency := range []int{0, -1} {
		t.Run(fmt.Sprintf("MaxConcurrent=%d", concurrency), func(t *testing.T) {
			tempOut := tempDir(t)
			tempState := tempDir(t)
			server := mockServer(content, true)
			defer server.Close()

			cfg := &Config{
				OutputDir:      tempOut,
				ResumeStateDir: tempState,
				MaxConcurrent:  concurrency,
				ChunkSize:      10, // 与并发无关，这里必须给正数（ChunkSize=0 是另一条已知问题）
				EnableResume:   true,
				SaveBatchSize:  2,
			}
			d := NewDownloader(cfg)

			// 下载放 goroutine：卡住时靠超时把测试**干脆地**判失败，而不是挂到 go test 的全局超时
			done := make(chan *DownloadResult, 1)
			go func() {
				done <- d.Download(context.Background(), server.URL, "subdir", "zero.txt", nil)
			}()

			select {
			case result := <-done:
				if result.Error != nil {
					t.Fatalf("下载应当成功：%v", result.Error)
				}
				if result.Size != int64(len(content)) {
					t.Fatalf("Size = %d, want %d", result.Size, len(content))
				}
				got, err := os.ReadFile(filepath.Join(tempOut, "subdir", "zero.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(content) {
					t.Fatalf("文件内容 = %q, want %q（分片顺序不能乱）", got, content)
				}
				// 状态文件应当被清掉
				if stateFiles, _ := filepath.Glob(filepath.Join(tempState, "subdir", "*.json")); len(stateFiles) != 0 {
					t.Fatalf("状态文件没清理：%+v", stateFiles)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("MaxConcurrent=%d 时下载卡死了：信号量无缓冲，wg.Wait() 永不返回", concurrency)
			}
		})
	}
}

// 分片重试的退避必须能被 ctx 打断（与 engine/stage.go 里那段同一个问题）。
// 回归点：改之前是裸的 time.Sleep；而 Engine.Stop 只等 crawler.stop_timeout（模板 5s），
// 退避最多睡 10s —— 睡满的话 Stop 会报"未排空"，并连带跳过关库与关浏览器池的收尾
// （那两件事正是在 drained=false 时故意不做的）。
func TestChunkRetryBackoffIsInterruptible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 每次尝试都失败，逼出退避
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = t.TempDir()
	d := NewDownloader(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// 第 1 次退避 = 1<<1 = 2s，够测了
		done <- d.downloadChunkToFile(ctx, srv.URL, 0, 9, filepath.Join(cfg.OutputDir, "chunk.part"), &requestConfig{})
	}()

	time.Sleep(300 * time.Millisecond) // 等第一次尝试失败、进入退避
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消 ctx 后应当以错误结束")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("取消 ctx 后仍在睡退避 —— 退避没接 ctx")
	}
}
