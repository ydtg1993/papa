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

	"github.com/ydtg1993/papa/v3/core"
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

// 取消下载要**及时退出** —— 这条用例的价值全在"取消是不是真的能打断挂住的下载"。
//
// 所以服务端**故意不返回响应体**（只有客户端取消才会解开那个 handler）：
//   - 下载器接 ctx → 取消后请求报错、Download 迅速返回 ✓
//   - 不接 ctx → 它会一直等下去，5s 的看门狗抓出来 ✗
//
// 最初这版用的是 mockServer + 50MB 文件，那是**假的**：本地 50MB 在 300ms 内就跑完了，
// `cancel()` 是个空操作，不接 ctx 也照样过；而且原来那句 `<-done` 没有上限，
// 真挂住只能等 Go 自己 10 分钟超时。临时分片的清理是尽力而为，只记不断言。
func TestCancelReturnsPromptly(t *testing.T) {
	// release 是**收尾兜底**：handler 卡在"等客户端取消"上，而 httptest.Server.Close()
	// 会等未完成的请求 —— 用例一旦失败（取消没生效），没人取消那个请求，
	// Close 就会跟着一起挂住，报出来的是一句超时而不是本用例的那句 Fatal。
	// 用 t.Cleanup（而不是 defer）是顺序要求：它在测试函数的 defer 之后跑，
	// 所以 t.Fatal 之后仍会先 close(release) 放掉 handler，再 srv.Close()。
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", "52428800") // 50MB，够分几片
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case <-r.Context().Done(): // 正常路径：客户端取消，请求结束
		case <-release: // 兜底路径：用例失败时放行，别把 Close 拖住
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	tempOut := tempDir(t)
	tempState := tempDir(t)
	downloader := NewDownloader(&Config{
		OutputDir:      tempOut,
		ResumeStateDir: tempState,
		MaxConcurrent:  2,
		ChunkSize:      10 * 1024 * 1024,
		EnableResume:   true,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		downloader.Download(ctx, srv.URL, "cancel", "bigfile.bin", nil)
	}()

	time.Sleep(300 * time.Millisecond) // 等它进到分片下载（此时每个请求都在等响应体）
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消之后 Download 没在 5s 内返回 —— 取消路径没接 ctx")
	}

	// 清理是尽力而为，不作断言（断言了会偶发红，红了又会被当成噪声关掉）
	if _, err := os.Stat(filepath.Join(tempState, "segments", "cancel", "bigfile.bin")); err == nil {
		t.Log("temp segments may still exist（尽力而为的清理，不作断言）")
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

// 成功的下载不该往错误通道写任何东西 —— 尤其不能拿它传进度。
//
// 回归点：分片循环里每完成一个分片就 `SendError("分片 x/y 完成")`，而错误通道那端按
// **Error 级别**落盘（`internal/app` 的 mdMsgListener）—— 于是"成功"被写成了 ERROR：
// 实测下 11 张封面就是 11 行 `level=error`，而且业务侧关不掉（只要用 filedown 就有）。
// 进度本来就有 `OnProgress` 这个通道，不该借错误通道。
func TestSuccessfulDownloadKeepsErrorChannelQuiet(t *testing.T) {
	tempOut := tempDir(t)
	tempState := tempDir(t)
	content := []byte("abcdefghijklmnopqrstuvwxyz1234567890") // 36 字节 / 10 = 4 个分片
	server := mockServer(content, true)
	defer server.Close()

	var progress int
	cfg := &Config{
		OutputDir: tempOut, ResumeStateDir: tempState,
		MaxConcurrent: 2, ChunkSize: 10, EnableResume: true, SaveBatchSize: 2,
		QueueSize:  100, // 装得下所有分片的消息，免得"满即丢"把这条用例测糊
		OnProgress: func(int64, int64) { progress++ },
	}
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL, "subdir", "test.txt", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %s", result.Error.Error())
	}
	if progress == 0 {
		t.Fatal("进度回调没被调用 —— 那这条用例根本没覆盖到分片循环")
	}

	select {
	case err := <-downloader.GetErrors():
		t.Fatalf("成功的下载不该往错误通道写东西，实得：%v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// ContentType 要从响应里带出来：URL 的后缀在图片代理那种场景下并不可信
// （源图 webp、按 Accept 协商后返回 jpeg，照后缀存就是"后缀与内容不符"）。
// 两条路都要带：分片下载（取自 HEAD 探测）与单线程降级（取自那一次 GET）。
func TestDownloadResultCarriesContentType(t *testing.T) {
	content := []byte("abcdefghijklmnopqrstuvwxyz1234567890")

	for _, tc := range []struct {
		name         string
		supportRange bool
	}{
		{"分片", true},
		{"单线程", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "image/webp")
				if r.Method == "HEAD" {
					if tc.supportRange {
						w.Header().Set("Accept-Ranges", "bytes")
					}
					w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
					w.WriteHeader(http.StatusOK)
					return
				}
				if tc.supportRange && r.Header.Get("Range") != "" {
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
					_, _ = w.Write(content[start : end+1])
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(content)
			}))
			defer server.Close()

			cfg := DefaultConfig()
			cfg.OutputDir = tempDir(t)
			cfg.ResumeStateDir = tempDir(t)
			cfg.ChunkSize = 10
			cfg.MaxConcurrent = 2
			downloader := NewDownloader(cfg)

			result := downloader.Download(context.Background(), server.URL, "subdir", "cover.img", nil)
			if result.Error != nil {
				t.Fatalf("download failed: %v", result.Error)
			}
			if result.ContentType != "image/webp" {
				t.Fatalf("ContentType = %q, want image/webp（调用方要靠它核对后缀）", result.ContentType)
			}
			if result.Size != int64(len(content)) {
				t.Fatalf("Size = %d, want %d", result.Size, len(content))
			}
		})
	}
}

// 逐下载指定出口：同一个代理地址复用同一个客户端（连接池不白扔），不同地址互不影响。
func TestDownloadOptionsProxySelectsClient(t *testing.T) {
	d := NewDownloader(DefaultConfig())

	plain := d.buildRequestConfig(nil)
	if d.clientOf(plain) != d.client {
		t.Fatal("没指定代理就该用下载器默认的客户端")
	}

	cfg := d.buildRequestConfig(&DownloadOptions{Proxy: "http://1.2.3.4:8080"})
	client := d.clientOf(cfg)
	if client == d.client {
		t.Fatal("指定了代理就该换个客户端")
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("客户端应当带代理 transport，实得 %#v", client.Transport)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.String() != "http://1.2.3.4:8080" {
		t.Fatalf("transport 应当把请求路由到指定代理，实得 %v %v", u, err)
	}
	// 同一个地址复用
	if again := d.clientOf(d.buildRequestConfig(&DownloadOptions{Proxy: "http://1.2.3.4:8080"})); again != client {
		t.Fatal("同一个代理地址应当复用客户端")
	}
	// 另一个地址是另一个客户端
	if other := d.clientOf(d.buildRequestConfig(&DownloadOptions{Proxy: "http://5.6.7.8:3128"})); other == client {
		t.Fatal("不同代理地址不该共用客户端")
	}
	// 地址写错：回落默认客户端 + 错误通道留痕（别静默变成直连）
	bad := d.buildRequestConfig(&DownloadOptions{Proxy: "://坏地址"})
	if d.clientOf(bad) != d.client {
		t.Fatal("地址解析不出来时应回落默认客户端")
	}
	select {
	case err := <-d.GetErrors():
		if !strings.Contains(err.Error(), "parse proxy URL") {
			t.Fatalf("错误通道上应留痕，实得 %v", err)
		}
	default:
		t.Fatal("代理地址解析失败应当在错误通道留痕")
	}
}

// Direct：小文件直下 —— 一次 GET 落盘，**不先发 HEAD**，也不走分片机器。
func TestDownloadOptionsDirectSkipsHeadAndChunks(t *testing.T) {
	content := []byte("small image bytes")
	var heads, gets int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads++
			w.Header().Set("Accept-Ranges", "bytes") // 就算支持分片，Direct 也不该走那条路
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			return
		}
		gets++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.ChunkSize = 4 // 默认路径会切成 5 个分片；Direct 应当只有 1 个 GET
	downloader := NewDownloader(cfg)

	res := downloader.Download(context.Background(), server.URL, "subdir", "cover.img", &DownloadOptions{Direct: true})
	if res.Error != nil {
		t.Fatalf("download failed: %v", res.Error)
	}
	if heads != 0 {
		t.Fatalf("Direct 不该先发 HEAD，实得 %d 次", heads)
	}
	if gets != 1 {
		t.Fatalf("Direct 应当只有一次 GET，实得 %d 次", gets)
	}
	if res.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", res.Size, len(content))
	}
	// 走的是单线程那条路：Content-Type 取自那次 GET
	if res.ContentType == "" {
		t.Fatal("Direct 应当把响应声明的 Content-Type 带出来")
	}
	data, err := os.ReadFile(filepath.Join(cfg.OutputDir, res.OutputFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(content) {
		t.Fatalf("文件内容 = %q, want %q", data, content)
	}
}

// OutputDir() 是 DownloadResult.OutputFile 的基准：两者一拼就是那个文件。调用方要把
// 「相对路径」变成能落库、能在后台目录里看到的本地路径，只能靠这个 getter —— 若自己再写一个
// 常量去对齐 Config.OutputDir，两处漂了**不报错**，只表现为「文件下下来了但记的路径指空」。
func TestOutputDirGetterIsTheBaseOfOutputFile(t *testing.T) {
	content := []byte("cover-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL+"/cover.img", "covers/series", "abc123.img", &DownloadOptions{Direct: true})
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}

	if got := downloader.OutputDir(); got != cfg.OutputDir {
		t.Fatalf("OutputDir() = %q, want %q（配置里写了什么就返回什么）", got, cfg.OutputDir)
	}
	// OutputFile 是**相对** OutputDir 的：拼起来必须正好落在下载出来的那个文件上
	abs := filepath.Join(downloader.OutputDir(), result.OutputFile)
	if got, err := os.ReadFile(abs); err != nil || string(got) != string(content) {
		t.Fatalf("拼出来的路径 %q 读不到刚下的内容（err=%v, got=%q）", abs, err, got)
	}
}

// 没传配置时走 DefaultConfig（NewDownloader 自己补的），getter 不能返回空串 ——
// 拼路径时让它变成空串等于把文件写到相对目录的根上去。
func TestOutputDirGetterDefaultsWhenConfigNil(t *testing.T) {
	if got, want := NewDownloader(nil).OutputDir(), DefaultConfig().OutputDir; got != want {
		t.Fatalf("OutputDir() = %q, want %q", got, want)
	}
}

// 下载器不读 ctx（隐式继承会让人不知道请求上到底带了什么）：抓取上下文要显式转成下载选项。
// OptionsFromRequest 负责把站点级 / 逐请求头与显式代理带过来，其余（Direct、Accept 之类）留给自己补。
func TestOptionsFromRequestCarriesHeadersAndProxy(t *testing.T) {
	ctx := core.WithHeaders(context.Background(), map[string]string{
		"User-Agent": "ua-site",
		"Cookie":     "sid=1",
		"X-Drop":     "", // 空值 = 删掉这个头
	})
	ctx = core.WithProxyURL(ctx, "http://127.0.0.1:8080")

	opts := OptionsFromRequest(ctx, "https://site.example/detail/1")
	if opts.Referer != "https://site.example/detail/1" {
		t.Fatalf("Referer = %q", opts.Referer)
	}
	if opts.Proxy != "http://127.0.0.1:8080" {
		t.Fatalf("Proxy = %q，ctx 上指定了出口就该带上", opts.Proxy)
	}
	if opts.Headers["User-Agent"] != "ua-site" || opts.Headers["Cookie"] != "sid=1" {
		t.Fatalf("ctx 上的头没带过来：%v", opts.Headers)
	}
	// 空值 = "删掉这个头"（与抓取路径同一套语义），不能变成发出去一个空头
	if _, ok := opts.Headers["X-Drop"]; ok {
		t.Fatalf("空值的头应当被删掉而不是留着：%v", opts.Headers)
	}

	// 头是副本：改选项不该影响 ctx 里那份（否则下一个下载会莫名其妙换 UA）
	opts.Headers["User-Agent"] = "tampered"
	if core.HeadersFrom(ctx)["User-Agent"] != "ua-site" {
		t.Fatal("OptionsFromRequest 返回的头应当是副本")
	}
}

// ctx 上什么都没有：只带 Referer；Headers 是**可写的空 map**而不是 nil ——
// 调用方紧接着就要补自己的键（`opts.Headers["Accept"] = ...`），给 nil map 就是等着 panic。
func TestOptionsFromRequestWithoutContext(t *testing.T) {
	opts := OptionsFromRequest(context.Background(), "https://site.example/p")
	if opts.Referer != "https://site.example/p" || opts.Proxy != "" {
		t.Fatalf("空 ctx 应当只带 Referer：%+v", opts)
	}
	if opts.Headers == nil || len(opts.Headers) != 0 {
		t.Fatalf("Headers 应当是非 nil 的空 map：%#v", opts.Headers)
	}
	opts.Headers["Accept"] = "image/webp" // 不该 panic
	if len(core.HeadersFrom(context.Background())) != 0 {
		t.Fatal("补键不该影响到 ctx")
	}
}

// 端到端：这些头真的落到下载请求上。ctx 里与参数里都有 Referer 时以**参数**为准
// （下载的来源页是详情页，而不是抓取那一下的地址）。
func TestOptionsFromRequestReachesTheRequest(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
	}))
	defer server.Close()

	ctx := core.WithHeaders(context.Background(), map[string]string{
		"User-Agent": "ua-site",
		"Cookie":     "sid=1",
		"Referer":    "https://ctx.example/",
	})
	opts := OptionsFromRequest(ctx, server.URL+"/detail/1")
	opts.Direct = true

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	res := NewDownloader(cfg).Download(ctx, server.URL+"/cover.img", "covers", "a.img", opts)
	if res.Error != nil {
		t.Fatalf("download failed: %v", res.Error)
	}
	if got.Get("User-Agent") != "ua-site" || got.Get("Cookie") != "sid=1" {
		t.Fatalf("请求上没有带上 ctx 的头：%v", got)
	}
	if got.Get("Referer") != server.URL+"/detail/1" {
		t.Fatalf("Referer = %q，参数里的来源页应当优先于 ctx 里那个", got.Get("Referer"))
	}
}
