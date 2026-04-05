package filedown

import (
	"context"
	"fmt"
	_ "mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	pkg2 "github.com/ydtg1993/papa/pkg"
)

// Downloader 文件下载器（并发安全，无状态）
type Downloader struct {
	config     *Config
	client     *http.Client
	trackQueue *pkg2.MsgQueue[any]
}

// NewDownloader 创建下载器
func NewDownloader(cfg *Config) *Downloader {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &Downloader{
		config: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		trackQueue: pkg2.NewMsgQueue[any](10),
	}
}

// DownloadOptions 单次下载选项
type DownloadOptions struct {
	UserAgent string
	Referer   string
	Cookie    string
	Headers   map[string]string
}

// DownloadResult 下载结果
type DownloadResult struct {
	OutputFile string
	Size       int64
	Error      error
}

// Download 并发安全下载
func (d *Downloader) Download(ctx context.Context, fileURL, fileName string, opts *DownloadOptions) *DownloadResult {
	reqCfg := d.buildRequestConfig(opts)

	// HEAD 探测
	totalSize, supportRange, contentDisposition := d.headFile(ctx, fileURL, reqCfg)
	if totalSize <= 0 || !supportRange {
		d.trackQueue.SendError(fmt.Errorf("不支持分片下载，降级为单线程"))
		return d.downloadSingle(ctx, fileURL, fileName, contentDisposition, reqCfg)
	}

	// 确定输出文件名
	if fileName == "" {
		fileName = d.genFileName(fileURL, contentDisposition)
	}
	fileName = filepath.Base(fileName)
	_ = os.MkdirAll(d.config.OutputDir, 0755)
	filePath := filepath.Join(d.config.OutputDir, fileName)

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	defer file.Close()
	_ = file.Truncate(totalSize)

	// 分片下载
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.config.MaxConcurrent)
	var downloadErr error
	var errMu sync.Mutex
	var downloaded int64
	var mu sync.Mutex

	for start := int64(0); start < totalSize; start += d.config.ChunkSize {
		end := start + d.config.ChunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(start, end int64) {
			defer wg.Done()
			defer func() { <-sem }()

			err := d.downloadChunk(ctx, fileURL, start, end, file, reqCfg, totalSize, &downloaded, &mu)
			if err != nil {
				errMu.Lock()
				if downloadErr == nil {
					downloadErr = err
				}
				errMu.Unlock()
			}
		}(start, end)
	}
	wg.Wait()

	if downloadErr != nil {
		_ = file.Close()
		_ = os.Remove(filePath)
		return &DownloadResult{Error: downloadErr}
	}

	return &DownloadResult{
		OutputFile: filePath,
		Size:       totalSize,
	}
}

// buildRequestConfig 合并全局配置与单次选项
func (d *Downloader) buildRequestConfig(opts *DownloadOptions) *requestConfig {
	cfg := &requestConfig{
		headers: make(map[string]string),
	}
	if opts == nil {
		return cfg
	}
	cfg.userAgent = opts.UserAgent
	cfg.referer = opts.Referer
	cfg.cookie = opts.Cookie
	for k, v := range opts.Headers {
		cfg.headers[k] = v
	}
	return cfg
}

// requestConfig 单次请求配置
type requestConfig struct {
	userAgent string
	referer   string
	cookie    string
	headers   map[string]string
}

// headFile 发送 HEAD 请求，返回文件大小、是否支持 Range、Content-Disposition
func (d *Downloader) headFile(ctx context.Context, fileURL string, cfg *requestConfig) (int64, bool, string) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", fileURL, nil)
	if err != nil {
		return 0, false, ""
	}
	d.applyHeadersToReq(req, cfg)

	resp, err := d.client.Do(req)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return 0, false, ""
	}
	defer resp.Body.Close()

	totalSize := resp.ContentLength
	supportRange := strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")
	contentDisposition := resp.Header.Get("Content-Disposition")
	return totalSize, supportRange, contentDisposition
}

// downloadChunk 下载单个分片，支持重试，实时更新进度
func (d *Downloader) downloadChunk(ctx context.Context, fileURL string, start, end int64, file *os.File,
	cfg *requestConfig, totalSize int64, downloaded *int64, mu *sync.Mutex) error {

	var lastErr error
	chunkSize := end - start + 1

	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
		// 检查 context 取消
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if attempt > 0 {
			sleepTime := time.Duration(1<<attempt) * time.Second
			if sleepTime > 10*time.Second {
				sleepTime = 10 * time.Second
			}
			time.Sleep(sleepTime)
		}

		req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		d.applyHeadersToReq(req, cfg)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("unexpected status: %d", resp.StatusCode)
			continue
		}

		buf := make([]byte, 128*1024)
		offset := start
		var chunkDownloaded int64
		success := true

		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := file.WriteAt(buf[:n], offset); writeErr != nil {
					lastErr = writeErr
					success = false
					break
				}
				offset += int64(n)
				chunkDownloaded += int64(n)

				mu.Lock()
				*downloaded += int64(n)
				if d.config.OnProgress != nil {
					d.config.OnProgress(*downloaded, totalSize)
				}
				mu.Unlock()
			}
			if readErr != nil {
				break
			}
		}
		_ = resp.Body.Close()
		if success {
			// 确保下载的字节数与分片大小一致
			if chunkDownloaded != chunkSize {
				// 理论上应该相等，若不相等则视为失败
				lastErr = fmt.Errorf("downloaded %d bytes, expected %d", chunkDownloaded, chunkSize)
				continue
			}
			return nil
		}
	}
	return fmt.Errorf("分片 [%d-%d] 失败: %w", start, end, lastErr)
}

// downloadSingle 单线程降级下载
func (d *Downloader) downloadSingle(ctx context.Context, fileURL, fileName, contentDisposition string, cfg *requestConfig) *DownloadResult {
	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	d.applyHeadersToReq(req, cfg)

	resp, err := d.client.Do(req)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	defer resp.Body.Close()

	if fileName == "" {
		fileName = d.genFileName(fileURL, contentDisposition)
	}
	fileName = filepath.Base(fileName)
	_ = os.MkdirAll(d.config.OutputDir, 0755)
	filePath := filepath.Join(d.config.OutputDir, fileName)

	file, err := os.Create(filePath)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	defer file.Close()

	buf := make([]byte, 128*1024)
	var written int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = file.Write(buf[:n])
			written += int64(n)
			if d.config.OnProgress != nil {
				d.config.OnProgress(written, resp.ContentLength)
			}
		}
		if readErr != nil {
			break
		}
	}
	return &DownloadResult{
		OutputFile: filePath,
		Size:       written,
	}
}

// applyHeadersToReq 应用请求头（仅当值非空时设置）
func (d *Downloader) applyHeadersToReq(req *http.Request, cfg *requestConfig) {
	if cfg.userAgent != "" {
		req.Header.Set("User-Agent", cfg.userAgent)
	}
	for k, v := range cfg.headers {
		req.Header.Set(k, v)
	}
	if cfg.referer != "" {
		req.Header.Set("Referer", cfg.referer)
	}
	if cfg.cookie != "" {
		req.Header.Set("Cookie", cfg.cookie)
	}
}

// genFileName 生成文件名（优先 Content-Disposition，其次 URL 路径，最后默认）
func (d *Downloader) genFileName(rawURL, contentDisposition string) string {
	// 1. Content-Disposition
	if contentDisposition != "" {
		if strings.Contains(contentDisposition, "filename=") {
			parts := strings.Split(contentDisposition, "filename=")
			if len(parts) > 1 {
				name := strings.Trim(parts[1], `"`)
				if name != "" {
					return filepath.Base(name)
				}
			}
		}
	}
	// 2. URL 路径
	u, err := url.Parse(rawURL)
	if err == nil {
		base := path.Base(u.Path)
		if base != "" && base != "/" && strings.Contains(base, ".") {
			return filepath.Base(base)
		}
	}
	// 3. 默认
	return fmt.Sprintf("file_%d", time.Now().UnixNano())
}

// GetErrors 获取错误通道
func (d *Downloader) GetErrors() <-chan error {
	return d.trackQueue.Errors()
}
