package m3u8

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
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

// Downloader 实例
type Downloader struct {
	config  *Config
	client  *http.Client
	mu      sync.RWMutex
	referer string
	cookie  string
	errors  chan error
}

// NewDownloader 创建下载器
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

// SetUserAgent 动态设置 UA
func (d *Downloader) SetUserAgent(ua string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.UserAgent = ua
}

// SetReferer 动态设置 Referer
func (d *Downloader) SetReferer(ref string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.referer = ref
}

// SetCookie 动态设置 Cookie
func (d *Downloader) SetCookie(cookie string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cookie = cookie
}

// SetHeader 设置单个 Header
func (d *Downloader) SetHeader(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.Headers == nil {
		d.config.Headers = make(map[string]string)
	}
	d.config.Headers[key] = value
}

// SetHeaders 批量设置 Headers
func (d *Downloader) SetHeaders(headers map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.Headers == nil {
		d.config.Headers = make(map[string]string)
	}
	for k, v := range headers {
		d.config.Headers[k] = v
	}
}

// GetErrors 获取错误通道
func (d *Downloader) GetErrors() <-chan error {
	return d.errors
}

func (d *Downloader) sendError(err error) {
	select {
	case d.errors <- err:
	default:
	}
}

// doRequest 执行 HTTP 请求，支持重试
func (d *Downloader) doRequest(ctx context.Context, url, rangeHeader string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
		if attempt > 0 {
			sleepTime := time.Duration(d.config.RetryInterval) * time.Second
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

		if rangeHeader != "" && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d (expected 206)", resp.StatusCode)
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

// RateLimiter 限速器
type RateLimiter struct {
	rate int64
}

func (r *RateLimiter) Wait(n int) {
	if r.rate <= 0 {
		return
	}
	sleep := time.Duration(int64(time.Second) * int64(n) / r.rate)
	time.Sleep(sleep)
}

// SegmentInfo 片段信息
type SegmentInfo struct {
	URL   string
	Range string
}

// KeyInfo 密钥信息
type KeyInfo struct {
	URL string
	IV  string
}

// DownloadResult 下载结果
type DownloadResult struct {
	OutputFile string
	Segments   int
	Size       int64
	Error      error
}

// Download 同步阻塞下载，保证顺序
func (d *Downloader) Download(ctx context.Context, m3u8URL, outputFile string) *DownloadResult {
	return d.download(ctx, m3u8URL, outputFile)
}

func (d *Downloader) download(ctx context.Context, m3u8URL, outputFile string) *DownloadResult {
	result := &DownloadResult{}

	// 1. 获取播放列表
	playlist, baseURL, err := d.fetchPlaylist(ctx, m3u8URL)
	if err != nil {
		result.Error = fmt.Errorf("fetch playlist: %w", err)
		return result
	}

	// 2. 处理多码率
	if strings.Contains(playlist, "#EXT-X-STREAM-INF") {
		bestURL, err := d.selectBestStream(playlist, baseURL)
		if err == nil {
			d.sendError(fmt.Errorf("选择最高码率流: %s", bestURL))
			playlist, baseURL, err = d.fetchPlaylist(ctx, bestURL)
			if err != nil {
				result.Error = fmt.Errorf("fetch best stream: %w", err)
				return result
			}
		}
	}

	// 3. 解析
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
		result.Error = fmt.Errorf("mkdir: %w", err)
		return result
	}

	// 5. 打开最终文件（覆盖或追加）
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	file, err := os.OpenFile(outputPath, flag, 0644)
	if err != nil {
		result.Error = fmt.Errorf("open output: %w", err)
		return result
	}
	defer file.Close()

	// 6. 写入初始化段（如果有）
	if initSegment != nil {
		data, err := d.downloadSegment(ctx, initSegment.URL)
		if err != nil {
			result.Error = fmt.Errorf("init segment: %w", err)
			return result
		}
		if _, err := file.Write(data); err != nil {
			result.Error = fmt.Errorf("write init: %w", err)
			return result
		}
	}

	// 7. 顺序下载每个片段
	limiter := &RateLimiter{rate: int64(d.config.RateKB) * 1024}
	var totalBytes int64
	for idx, segInfo := range segments {
		// 下载片段数据
		var data []byte
		if segInfo.Range != "" {
			data, err = d.downloadPartialSegment(ctx, segInfo.URL, segInfo.Range)
		} else {
			data, err = d.downloadSegment(ctx, segInfo.URL)
		}
		if err != nil {
			result.Error = fmt.Errorf("segment %d: %w", idx+1, err)
			return result
		}

		// 解密
		keyInfo := segmentKeys[idx]
		if keyInfo != nil {
			key, iv, err := d.prepareKey(ctx, keyInfo, idx)
			if err != nil {
				result.Error = fmt.Errorf("segment %d key: %w", idx+1, err)
				return result
			}
			data, err = d.decryptAES128CBC(data, key, iv)
			if err != nil {
				result.Error = fmt.Errorf("segment %d decrypt: %w", idx+1, err)
				return result
			}
		}

		// 限速
		limiter.Wait(len(data))

		// 写入文件
		if _, err := file.Write(data); err != nil {
			result.Error = fmt.Errorf("write segment %d: %w", idx+1, err)
			return result
		}
		totalBytes += int64(len(data))

		// 进度回调
		if d.config.OnProgress != nil {
			d.config.OnProgress(idx+1, len(segments), idx+1, int64(len(data)), totalBytes)
		}
		d.sendError(fmt.Errorf("片段 %d/%d 完成", idx+1, len(segments)))
	}

	result.OutputFile = outputPath
	result.Segments = len(segments)
	result.Size = totalBytes
	return result
}

// fetchPlaylist 获取 M3U8 内容及 base URL
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

func (d *Downloader) downloadSegment(ctx context.Context, url string) ([]byte, error) {
	return d.doRequest(ctx, url, "")
}

func (d *Downloader) downloadPartialSegment(ctx context.Context, url, byteRange string) ([]byte, error) {
	return d.doRequest(ctx, url, "bytes="+byteRange)
}

func (d *Downloader) prepareKey(ctx context.Context, keyInfo *KeyInfo, segmentIndex int) (key, iv []byte, err error) {
	key, err = d.doRequest(ctx, keyInfo.URL, "")
	if err != nil {
		return nil, nil, err
	}
	if keyInfo.IV != "" {
		iv, err = d.parseIV(keyInfo.IV)
	} else {
		iv = make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], uint64(segmentIndex))
	}
	return key, iv, err
}

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

func (d *Downloader) parseIV(ivStr string) ([]byte, error) {
	ivStr = strings.TrimPrefix(ivStr, "0x")
	if len(ivStr)%2 != 0 {
		ivStr = "0" + ivStr
	}
	return hex.DecodeString(ivStr)
}

// parsePlaylistEnhanced 解析 M3U8，返回 init 段、片段列表、每个片段对应的密钥
func (d *Downloader) parsePlaylistEnhanced(playlist, baseURL string) (*SegmentInfo, []*SegmentInfo, []*KeyInfo, error) {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	var initSegment *SegmentInfo
	var segments []*SegmentInfo
	var segmentKeys []*KeyInfo
	var currentKey *KeyInfo

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#EXT-X-VERSION") || strings.HasPrefix(line, "#EXT-X-TARGETDURATION") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			initSegment = d.parseMapTag(line, baseURL)
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			currentKey = d.parseKeyTag(line, baseURL)
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			byteRange := strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
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
		default:
			segURL := d.resolveURL(line, baseURL)
			segments = append(segments, &SegmentInfo{URL: segURL})
			segmentKeys = append(segmentKeys, currentKey)
		}
	}
	return initSegment, segments, segmentKeys, nil
}

func (d *Downloader) parseMapTag(tag, baseURL string) *SegmentInfo {
	uriStart := strings.Index(tag, "URI=\"")
	if uriStart == -1 {
		return nil
	}
	uriStart += 5
	uriEnd := strings.Index(tag[uriStart:], "\"")
	if uriEnd == -1 {
		return nil
	}
	uri := tag[uriStart : uriStart+uriEnd]
	url := d.resolveURL(uri, baseURL)
	return &SegmentInfo{URL: url}
}

func (d *Downloader) parseKeyTag(tag, baseURL string) *KeyInfo {
	uriStart := strings.Index(tag, "URI=\"")
	if uriStart == -1 {
		return nil
	}
	uriStart += 5
	uriEnd := strings.Index(tag[uriStart:], "\"")
	if uriEnd == -1 {
		return nil
	}
	uri := tag[uriStart : uriStart+uriEnd]
	url := d.resolveURL(uri, baseURL)
	iv := ""
	if ivStart := strings.Index(tag, "IV="); ivStart != -1 {
		ivPart := tag[ivStart+3:]
		ivEnd := strings.IndexAny(ivPart, ", \t\n\r")
		if ivEnd == -1 {
			iv = ivPart
		} else {
			iv = ivPart[:ivEnd]
		}
	}
	return &KeyInfo{URL: url, IV: iv}
}

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

// selectBestStream 选择最高码率流
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

// RemuxTS2MP4 简单复制（实际需用 ffmpeg）
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
