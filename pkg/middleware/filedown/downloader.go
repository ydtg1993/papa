package filedown

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type Downloader struct {
	config  *Config
	client  *http.Client
	mu      sync.RWMutex
	log     *logrus.Logger
	referer string
	cookie  string
}

func NewDownloader(cfg *Config) *Downloader {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if cfg.Logger == nil {
		cfg.Logger = logrus.New()
	}

	return &Downloader{
		config: cfg,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
		log: cfg.Logger,
	}
}

func (d *Downloader) SetHeader(k, v string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.Headers[k] = v
}

func (d *Downloader) SetReferer(v string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.referer = v
}

func (d *Downloader) SetCookie(v string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cookie = v
}

func (d *Downloader) applyHeaders(req *http.Request) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	req.Header.Set("User-Agent", d.config.UserAgent)
	for k, v := range d.config.Headers {
		req.Header.Set(k, v)
	}
	if d.referer != "" {
		req.Header.Set("Referer", d.referer)
	}
	if d.cookie != "" {
		req.Header.Set("Cookie", d.cookie)
	}
}

type DownloadResult struct {
	OutputFile string
	Size       int64
	Error      error
}

func (d *Downloader) Download(ctx context.Context, fileURL, fileName string) *DownloadResult {
	result := &DownloadResult{}

	// ===== HEAD =====
	req, _ := http.NewRequestWithContext(ctx, "HEAD", fileURL, nil)
	d.applyHeaders(req)

	resp, err := d.client.Do(req)
	if err != nil || resp.StatusCode >= 400 {
		d.log.Warn("HEAD失败，降级为单线程")
		return d.downloadSingle(ctx, fileURL, fileName)
	}
	defer resp.Body.Close()

	totalSize := resp.ContentLength
	supportRange := strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes")

	if totalSize <= 0 || !supportRange {
		d.log.Info("不支持分片下载")
		return d.downloadSingle(ctx, fileURL, fileName)
	}

	// ===== 文件准备 =====
	if fileName == "" {
		fileName = d.genFileName(fileURL, resp)
	}
	fileName = filepath.Base(fileName)

	_ = os.MkdirAll(d.config.OutputDir, 0755)
	filePath := filepath.Join(d.config.OutputDir, fileName)

	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		result.Error = err
		return result
	}
	defer file.Close()

	_ = file.Truncate(totalSize)

	// ===== 分片下载 =====
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.config.MaxConcurrent)

	var downloaded int64
	var mu sync.Mutex
	var downloadErr error
	var errMu sync.Mutex

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

			for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
				req, _ := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
				d.applyHeaders(req)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

				resp, err := d.client.Do(req)
				if err != nil {
					time.Sleep(time.Second)
					continue
				}

				if resp.StatusCode != http.StatusPartialContent {
					_ = resp.Body.Close()
					continue
				}

				buf := make([]byte, 128*1024)
				var offset = start

				for {
					n, err := resp.Body.Read(buf)
					if n > 0 {
						_, _ = file.WriteAt(buf[:n], offset)
						offset += int64(n)

						mu.Lock()
						downloaded += int64(n)
						if d.config.OnProgress != nil {
							d.config.OnProgress(downloaded, totalSize)
						}
						mu.Unlock()
					}
					if err != nil {
						break
					}
				}

				_ = resp.Body.Close()
				return
			}
			// 所有重试失败，记录错误
			errMu.Lock()
			if downloadErr == nil {
				downloadErr = fmt.Errorf("分片失败: %d-%d", start, end)
			}
			errMu.Unlock()
		}(start, end)
	}

	wg.Wait()
	// 检查错误并清理不完整文件
	if downloadErr != nil {
		_ = file.Close()        // 确保文件句柄关闭
		_ = os.Remove(filePath) // 删除不完整文件
		result.Error = downloadErr
		return result
	}
	result.OutputFile = filePath
	result.Size = totalSize
	return result
}

func (d *Downloader) downloadSingle(ctx context.Context, fileURL, fileName string) *DownloadResult {
	result := &DownloadResult{}

	req, _ := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	d.applyHeaders(req)

	resp, err := d.client.Do(req)
	if err != nil {
		result.Error = err
		return result
	}
	defer resp.Body.Close()
	if fileName == "" {
		fileName = d.genFileName(fileURL, resp)
	}
	fileName = filepath.Base(fileName)
	_ = os.MkdirAll(d.config.OutputDir, 0755)
	filePath := filepath.Join(d.config.OutputDir, fileName)

	file, err := os.Create(filePath)
	if err != nil {
		result.Error = err
		return result
	}
	defer file.Close()

	buf := make([]byte, 128*1024)
	var written int64

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = file.Write(buf[:n])
			written += int64(n)

			if d.config.OnProgress != nil {
				d.config.OnProgress(written, resp.ContentLength)
			}
		}
		if err != nil {
			break
		}
	}

	result.OutputFile = filePath
	result.Size = written
	return result
}

func (d *Downloader) genFileName(rawURL string, resp *http.Response) string {
	// ===== 1. Content-Disposition =====
	if resp != nil {
		cd := resp.Header.Get("Content-Disposition")
		if cd != "" {
			// 常见格式: attachment; filename="xxx.jpg"
			if strings.Contains(cd, "filename=") {
				parts := strings.Split(cd, "filename=")
				if len(parts) > 1 {
					name := strings.Trim(parts[1], `"`)
					if name != "" {
						return filepath.Base(name)
					}
				}
			}
		}
	}

	// ===== 2. URL path =====
	u, err := url.Parse(rawURL)
	if err == nil {
		base := path.Base(u.Path)

		// 必须包含扩展名才可信
		if base != "" && base != "/" && strings.Contains(base, ".") {
			return filepath.Base(base)
		}
	}

	// ===== 3. Content-Type 推断扩展名 =====
	ext := ""
	if resp != nil {
		ct := resp.Header.Get("Content-Type")
		ext = guessExtFromContentType(ct)
	}

	// ===== 4. fallback =====
	return fmt.Sprintf("file_%d%s", time.Now().UnixNano(), ext)
}

func guessExtFromContentType(ct string) string {
	ct = strings.ToLower(ct)

	switch {
	case strings.Contains(ct, "image/jpeg"):
		return ".jpg"
	case strings.Contains(ct, "image/png"):
		return ".png"
	case strings.Contains(ct, "image/gif"):
		return ".gif"
	case strings.Contains(ct, "image/webp"):
		return ".webp"
	case strings.Contains(ct, "application/pdf"):
		return ".pdf"
	case strings.Contains(ct, "application/zip"):
		return ".zip"
	case strings.Contains(ct, "video/mp4"):
		return ".mp4"
	default:
		return ""
	}
}
