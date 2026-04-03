package m3u8

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	_ "sync/atomic"
	"time"
)

// ======================== 下载器 ========================

// Downloader M3U8 下载器实例
type Downloader struct {
	config  *Config
	client  *http.Client
	mu      sync.RWMutex // 保护动态设置的请求头
	referer string       // 动态 Referer
	cookie  string       // 动态 Cookie
	errors  chan error   //错误消息队列
}

// NewDownloader 创建下载器实例
func NewDownloader(cfg *Config) *Downloader {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &Downloader{
		config: cfg,
		client: &http.Client{
			Timeout: cfg.SegmentTimeout,
		},
		errors: make(chan error, 10),
	}
}

// ======================== 动态设置请求头（与文件下载器风格一致） ========================

// SetUserAgent 动态设置 User-Agent
func (d *Downloader) SetUserAgent(ua string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.UserAgent = ua
	d.sendError(fmt.Errorf("User-Agent 已更新: %s", ua))
}

// SetReferer 动态设置 Referer
func (d *Downloader) SetReferer(ref string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.referer = ref
	d.sendError(fmt.Errorf("Referer 已更新: %s", ref))
}

// SetCookie 动态设置 Cookie
func (d *Downloader) SetCookie(cookie string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cookie = cookie
	d.sendError(fmt.Errorf("Cookie 已更新: %s", cookie))
}

// SetHeader 动态设置单个自定义 Header（会覆盖同名的已有 Header）
func (d *Downloader) SetHeader(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.Headers == nil {
		d.config.Headers = make(map[string]string)
	}
	d.config.Headers[key] = value
	d.sendError(fmt.Errorf("Header 已设置: %s: %s", key, value))
}

// SetHeaders 批量设置 Headers（合并方式，覆盖同名）
func (d *Downloader) SetHeaders(headers map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.Headers == nil {
		d.config.Headers = make(map[string]string)
	}
	for k, v := range headers {
		d.config.Headers[k] = v
	}
	d.sendError(fmt.Errorf("批量设置 %d 个 Headers", len(headers)))
}

// ======================== HTTP 请求（统一处理重试、限速、请求头） ========================

// doRequest 执行 HTTP 请求，支持 Range 头，自动重试（指数退避）
// 参数 ctx: 上下文，用于取消
// 参数 url: 请求地址
// 参数 rangeHeader: 可选，格式 "bytes=xxx"，为空表示不发送 Range 头
// 返回响应体 []byte 和错误
func (d *Downloader) doRequest(ctx context.Context, url string, rangeHeader string) ([]byte, error) {
	var lastErr error
	// 指数退避重试
	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
		if attempt > 0 {
			sleepTime := time.Duration(d.config.RetryInterval) * time.Second
			// 指数退避：2^attempt 秒，但不超过 10 秒
			if d.config.RetryInterval > 0 {
				sleepTime = time.Duration(1<<uint(attempt)) * time.Second
			}
			if sleepTime > 10*time.Second {
				sleepTime = 10 * time.Second
			}
			time.Sleep(sleepTime)
			d.sendError(fmt.Errorf("重试 %s (第 %d 次)", url, attempt))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		d.applyHeaders(req)
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		// 状态码检查
		if rangeHeader != "" && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d (expected 206 Partial Content)", resp.StatusCode)
			continue
		}
		if rangeHeader == "" && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, fmt.Errorf("failed after %d retries: %w", d.config.MaxRetries, lastErr)
}

// applyHeaders 为请求添加配置的 Headers 以及动态的 Referer/Cookie
func (d *Downloader) applyHeaders(req *http.Request) {
	d.mu.RLock()
	ua := d.config.UserAgent
	headers := d.config.Headers
	referer := d.referer
	cookie := d.cookie
	d.mu.RUnlock()

	req.Header.Set("User-Agent", ua)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

// ======================== 限速器 ========================

// RateLimiter 限速器，控制下载速率
type RateLimiter struct {
	rate int64 // 字节/秒
}

// Wait 根据数据大小等待相应时间
func (r *RateLimiter) Wait(n int) {
	if r.rate <= 0 {
		return
	}
	sleep := time.Duration(int64(time.Second) * int64(n) / r.rate)
	time.Sleep(sleep)
}

// ======================== 主下载流程 ========================

// SegmentInfo 片段信息，包含 URL 和字节范围（若有）
type SegmentInfo struct {
	URL   string
	Range string // 格式 "start-end" 或 "length@offset"
}

// KeyInfo 密钥信息，包含密钥 URL 和自定义 IV（若有）
type KeyInfo struct {
	URL string
	IV  string // 十六进制字符串，可能为空
}

// DownloadResult 下载结果
type DownloadResult struct {
	OutputFile string // 最终输出文件路径
	Segments   int    // 总片段数
	Size       int64  // 总字节数
	Error      error  // 错误信息
}

// Download 下载 M3U8 流（使用默认 context）
// 参数 m3u8URL: 播放列表地址
// 参数 outputFile: 可选，指定输出文件名（不含路径），默认从 URL 生成
// 返回 DownloadResult 指针
func (d *Downloader) Download(ctx context.Context, m3u8URL, outputFile string) *DownloadResult {
	return d.download(ctx, m3u8URL, outputFile)
}

// download 实际执行下载，支持上下文取消
func (d *Downloader) download(ctx context.Context, m3u8URL, outputFile string) *DownloadResult {
	result := &DownloadResult{}

	// 1. 获取播放列表
	playlist, baseURL, err := d.fetchPlaylist(ctx, m3u8URL)
	if err != nil {
		result.Error = fmt.Errorf("fetch playlist: %w", err)
		return result
	}

	// 2. 如果是主播放列表（带码率），自动选择最高码率
	if strings.Contains(playlist, "#EXT-X-STREAM-INF") {
		bestURL, err := d.selectBestStream(playlist, baseURL)
		if err == nil {
			d.sendError(fmt.Errorf("选择最高码率流: %s", bestURL))
			playlist, baseURL, err = d.fetchPlaylist(ctx, bestURL)
			if err != nil {
				result.Error = fmt.Errorf("fetch best stream playlist: %w", err)
				return result
			}
		} else {
			d.sendError(fmt.Errorf("自动选择最高码率失败，使用原始播放列表: %v", err))
		}
	}

	// 3. 解析播放列表
	initSegment, segments, segmentKeys, err := d.parsePlaylistEnhanced(playlist, baseURL)
	if err != nil {
		result.Error = fmt.Errorf("parse playlist: %w", err)
		return result
	}

	if len(segments) == 0 {
		result.Error = fmt.Errorf("no segments found")
		return result
	}

	// 4. 确定输出路径
	if outputFile == "" {
		outputFile = d.generateFileName(m3u8URL)
	}
	outputPath := filepath.Join(d.config.OutputDir, outputFile)
	if err := os.MkdirAll(d.config.OutputDir, 0755); err != nil {
		result.Error = fmt.Errorf("create output dir: %w", err)
		return result
	}

	// 5. 断点续传进度
	progressFile := outputPath + ".progress"
	downloadedSet := make(map[int]bool) // 已下载片段的索引集合
	var totalBytes int64
	if d.config.EnableResume {
		if err := d.loadProgress(progressFile, &downloadedSet, &totalBytes); err == nil {
			d.sendError(fmt.Errorf("断点续传: 已下载 %d/%d 片段，已接收 %d 字节",
				len(downloadedSet), len(segments), totalBytes))
		} else {
			d.sendError(fmt.Errorf("加载进度文件失败，将重新下载: %v", err))
			downloadedSet = make(map[int]bool)
			totalBytes = 0
		}
	}

	// 6. 下载初始化段（如果有）
	if initSegment != nil {
		if err := d.downloadInitSegment(ctx, initSegment, outputPath); err != nil {
			result.Error = fmt.Errorf("download init segment: %w", err)
			return result
		}
	}

	// 7. 打开输出文件（追加模式）
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		result.Error = fmt.Errorf("open output file: %w", err)
		return result
	}
	defer file.Close()

	// 8. 并发下载片段
	sem := make(chan struct{}, d.config.MaxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var downloadErr error
	completedCount := 0
	limiter := &RateLimiter{rate: int64(d.config.RateKB) * 1024}

	for i := 0; i < len(segments); i++ {
		if downloadedSet[i] {
			completedCount++
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 获取密钥信息
			keyInfo := segmentKeys[idx]
			var key []byte
			var iv []byte
			if keyInfo != nil {
				key, err = d.downloadKey(ctx, keyInfo.URL)
				if err != nil {
					mu.Lock()
					if downloadErr == nil {
						downloadErr = fmt.Errorf("segment %d: download key: %w", idx+1, err)
					}
					mu.Unlock()
					return
				}
				if keyInfo.IV != "" {
					iv, err = d.parseIV(keyInfo.IV)
					if err != nil {
						mu.Lock()
						if downloadErr == nil {
							downloadErr = fmt.Errorf("segment %d: parse IV: %w", idx+1, err)
						}
						mu.Unlock()
						return
					}
				} else {
					// 默认 IV：片段序号（大端序）
					iv = make([]byte, 16)
					binary.BigEndian.PutUint64(iv[8:], uint64(idx))
				}
			}

			// 下载片段
			segInfo := segments[idx]
			var data []byte
			if segInfo.Range != "" {
				data, err = d.downloadPartialSegment(ctx, segInfo.URL, segInfo.Range)
			} else {
				data, err = d.downloadSegment(ctx, segInfo.URL)
			}
			if err != nil {
				mu.Lock()
				if downloadErr == nil {
					downloadErr = fmt.Errorf("segment %d: download: %w", idx+1, err)
				}
				mu.Unlock()
				return
			}

			// 解密
			if key != nil {
				data, err = d.decryptAES128CBC(data, key, iv)
				if err != nil {
					mu.Lock()
					if downloadErr == nil {
						downloadErr = fmt.Errorf("segment %d: decrypt: %w", idx+1, err)
					}
					mu.Unlock()
					return
				}
			}

			// 限速
			limiter.Wait(len(data))

			// 写入文件
			mu.Lock()
			defer mu.Unlock()
			if _, err := file.Write(data); err != nil {
				if downloadErr == nil {
					downloadErr = fmt.Errorf("write segment %d: %w", idx+1, err)
				}
				return
			}
			totalBytes += int64(len(data))

			// 更新进度
			downloadedSet[idx] = true
			if d.config.EnableResume {
				_ = d.saveProgress(progressFile, downloadedSet, totalBytes)
			}
			completedCount++
			if d.config.OnProgress != nil {
				d.config.OnProgress(completedCount, len(segments), idx+1, int64(len(data)), totalBytes)
			}
			d.sendError(fmt.Errorf("片段 %d/%d 完成", idx+1, len(segments)))
		}(i)
	}
	wg.Wait()

	if downloadErr != nil {
		// 下载失败，清理不完整文件
		file.Close()
		d.sendError(fmt.Errorf("下载失败，删除不完整文件: %s", outputPath))
		os.Remove(outputPath)
		os.Remove(progressFile)
		result.Error = downloadErr
		return result
	}

	// 9. 清理进度文件
	os.Remove(progressFile)
	result.OutputFile = outputPath
	result.Segments = len(segments)
	result.Size = totalBytes
	return result
}

// ======================== 辅助下载函数 ========================

// fetchPlaylist 获取 M3U8 内容并计算 base URL（用于相对路径）
func (d *Downloader) fetchPlaylist(ctx context.Context, m3u8URL string) (string, string, error) {
	data, err := d.doRequest(ctx, m3u8URL, "")
	if err != nil {
		return "", "", err
	}
	playlist := string(data)

	u, err := neturl.Parse(m3u8URL)
	if err != nil {
		return "", "", err
	}
	baseURL := u.Scheme + "://" + u.Host + path.Dir(u.Path) + "/"
	return playlist, baseURL, nil
}

// downloadSegment 下载普通片段（无字节范围）
func (d *Downloader) downloadSegment(ctx context.Context, url string) ([]byte, error) {
	return d.doRequest(ctx, url, "")
}

// downloadPartialSegment 下载部分片段（支持 #EXT-X-BYTERANGE）
func (d *Downloader) downloadPartialSegment(ctx context.Context, url, byteRange string) ([]byte, error) {
	return d.doRequest(ctx, url, "bytes="+byteRange)
}

// downloadKey 下载 AES 密钥文件
func (d *Downloader) downloadKey(ctx context.Context, keyURL string) ([]byte, error) {
	return d.doRequest(ctx, keyURL, "")
}

// downloadInitSegment 下载初始化段并写入输出文件开头
func (d *Downloader) downloadInitSegment(ctx context.Context, seg *SegmentInfo, outputPath string) error {
	data, err := d.downloadSegment(ctx, seg.URL)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(data)
	return err
}

// ======================== AES-128-CBC 解密 ========================

// decryptAES128CBC AES-128-CBC 解密，并去除 PKCS#7 填充
func (d *Downloader) decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not multiple of block size")
	}
	plaintext := make([]byte, len(ciphertext))
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(plaintext, ciphertext)

	// 去除 PKCS#7 填充
	if len(plaintext) > 0 {
		paddingLen := int(plaintext[len(plaintext)-1])
		if paddingLen <= aes.BlockSize && paddingLen > 0 {
			plaintext = plaintext[:len(plaintext)-paddingLen]
		}
	}
	return plaintext, nil
}

// parseIV 将十六进制字符串形式的 IV 转换为字节数组
func (d *Downloader) parseIV(ivStr string) ([]byte, error) {
	ivStr = strings.TrimPrefix(ivStr, "0x")
	if len(ivStr)%2 != 0 {
		ivStr = "0" + ivStr
	}
	return hex.DecodeString(ivStr)
}

// ======================== M3U8 解析 ========================

// parsePlaylistEnhanced 增强版解析，返回初始化段、片段列表、每个片段的密钥信息
func (d *Downloader) parsePlaylistEnhanced(playlist, baseURL string) (initSegment *SegmentInfo, segments []*SegmentInfo, segmentKeys []*KeyInfo, err error) {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	var currentKey *KeyInfo

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			initSegment, err = d.parseMapTag(line, baseURL)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("parse MAP: %w", err)
			}
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			currentKey, err = d.parseKeyTag(line, baseURL)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("parse KEY: %w", err)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			byteRange := strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			// 下一行是片段 URL
			if scanner.Scan() {
				urlLine := strings.TrimSpace(scanner.Text())
				if !strings.HasPrefix(urlLine, "#") {
					segURL := d.resolveURL(urlLine, baseURL)
					segments = append(segments, &SegmentInfo{URL: segURL, Range: byteRange})
					segmentKeys = append(segmentKeys, currentKey)
				}
			}
		case strings.HasPrefix(line, "#"):
			// 其他标签忽略
			continue
		default:
			segURL := d.resolveURL(line, baseURL)
			segments = append(segments, &SegmentInfo{URL: segURL})
			segmentKeys = append(segmentKeys, currentKey)
		}
	}
	if len(segments) == 0 {
		return nil, nil, nil, fmt.Errorf("no segments found")
	}
	return initSegment, segments, segmentKeys, nil
}

// parseMapTag 解析 #EXT-X-MAP 标签，返回初始化段信息
func (d *Downloader) parseMapTag(tag, baseURL string) (*SegmentInfo, error) {
	uriStart := strings.Index(tag, "URI=\"")
	if uriStart == -1 {
		return nil, fmt.Errorf("URI not found")
	}
	uriStart += 5
	uriEnd := strings.Index(tag[uriStart:], "\"")
	if uriEnd == -1 {
		return nil, fmt.Errorf("URI not closed")
	}
	uri := tag[uriStart : uriStart+uriEnd]
	url := d.resolveURL(uri, baseURL)
	return &SegmentInfo{URL: url}, nil
}

// parseKeyTag 解析 #EXT-X-KEY 标签，返回密钥信息
func (d *Downloader) parseKeyTag(tag, baseURL string) (*KeyInfo, error) {
	uriStart := strings.Index(tag, "URI=\"")
	if uriStart == -1 {
		return nil, fmt.Errorf("URI not found")
	}
	uriStart += 5
	uriEnd := strings.Index(tag[uriStart:], "\"")
	if uriEnd == -1 {
		return nil, fmt.Errorf("URI not closed")
	}
	uri := tag[uriStart : uriStart+uriEnd]
	url := d.resolveURL(uri, baseURL)
	iv := ""
	if ivStart := strings.Index(tag, "IV="); ivStart != -1 {
		ivPart := tag[ivStart+3:]
		// IV 可能被逗号、空格或结尾分隔
		ivEnd := strings.IndexAny(ivPart, ", \t\n\r")
		if ivEnd == -1 {
			iv = ivPart
		} else {
			iv = ivPart[:ivEnd]
		}
	}
	return &KeyInfo{URL: url, IV: iv}, nil
}

// resolveURL 将相对路径解析为绝对 URL
func (d *Downloader) resolveURL(raw, base string) string {
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return raw
	}
	if base == "" {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		u, err := neturl.Parse(base)
		if err == nil {
			u.Path = raw
			return u.String()
		}
	}
	return base + raw
}

// selectBestStream 从主播放列表中选择最高码率的流
func (d *Downloader) selectBestStream(playlist, baseURL string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	var bestURL string
	maxBW := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			bw := extractBandwidth(line)
			if scanner.Scan() {
				urlLine := strings.TrimSpace(scanner.Text())
				if !strings.HasPrefix(urlLine, "#") {
					url := d.resolveURL(urlLine, baseURL)
					if bw > maxBW {
						maxBW = bw
						bestURL = url
					}
				}
			}
		}
	}
	if bestURL == "" {
		return "", fmt.Errorf("no stream found")
	}
	return bestURL, nil
}

// extractBandwidth 从 #EXT-X-STREAM-INF 行提取 BANDWIDTH 值
func extractBandwidth(line string) int {
	if !strings.Contains(line, "BANDWIDTH=") {
		return 0
	}
	parts := strings.Split(line, "BANDWIDTH=")
	if len(parts) < 2 {
		return 0
	}
	valStr := strings.Split(parts[1], ",")[0]
	bw, err := strconv.Atoi(valStr)
	if err != nil {
		return 0
	}
	return bw
}

// ======================== 文件名生成 ========================

// generateFileName 根据 M3U8 URL 生成输出文件名
func (d *Downloader) generateFileName(m3u8URL string) string {
	u, err := neturl.Parse(m3u8URL)
	if err != nil {
		return fmt.Sprintf("video_%d.ts", time.Now().Unix())
	}
	base := filepath.Base(u.Path)
	base = strings.TrimSuffix(base, ".m3u8")
	if base == "" {
		base = fmt.Sprintf("video_%d", time.Now().Unix())
	}
	return base + ".ts"
}

// ======================== 断点续传进度文件 ========================

// progressData 进度文件 JSON 结构
type progressData struct {
	Downloaded map[int]bool `json:"downloaded"`  // 已下载片段索引
	TotalBytes int64        `json:"total_bytes"` // 累计已下载字节数
}

// saveProgress 保存进度到文件
func (d *Downloader) saveProgress(file string, downloaded map[int]bool, totalBytes int64) error {
	data, err := json.Marshal(progressData{Downloaded: downloaded, TotalBytes: totalBytes})
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0644)
}

// loadProgress 从文件加载进度
func (d *Downloader) loadProgress(file string, downloaded *map[int]bool, totalBytes *int64) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var pd progressData
	if err := json.Unmarshal(data, &pd); err != nil {
		return err
	}
	*downloaded = pd.Downloaded
	*totalBytes = pd.TotalBytes
	return nil
}

// ======================== 辅助功能：TS 转 MP4 简单封装 ========================

// RemuxTS2MP4 将 TS 文件直接复制为 MP4 文件（仅做容器格式转换，实际编码不变）
// 注意：此函数只是简单复制，并非真正的转封装，仅适用于某些场景。对于真正的转换，请使用 ffmpeg 等工具。
func RemuxTS2MP4(tsFile, mp4File string) error {
	in, err := os.Open(tsFile)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(mp4File)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// GetErrors 获取错误消息队列
func (d *Downloader) GetErrors() <-chan error {
	return d.errors
}

// sendError非阻塞发送错误
func (d *Downloader) sendError(err error) {
	select {
	case d.errors <- err:
	default:
		break
	}
}
