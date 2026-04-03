package m3u8

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	stateMu sync.Mutex // 保护状态文件写入
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

// SegmentInfo 片段信息（增加 Index 字段）
type SegmentInfo struct {
	URL   string
	Range string
	Index int // 0-based 索引
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

// ==================== 断点续传状态管理 ====================

type ResumeState struct {
	M3U8URL       string   `json:"m3u8_url"`
	OutputFile    string   `json:"output_file"`
	TotalSegments int      `json:"total_segments"`
	Completed     []int    `json:"completed"`     // 已完成的片段索引
	SegmentFiles  []string `json:"segment_files"` // 每个片段的临时文件路径（索引对应）
}

func (d *Downloader) getStateFilePath(m3u8URL, outputFile string) string {
	hash := fmt.Sprintf("%x", md5.Sum([]byte(m3u8URL+"|"+outputFile)))
	return filepath.Join(d.config.ResumeStateDir, hash+".json")
}

func (d *Downloader) loadResumeState(m3u8URL, outputFile string) (*ResumeState, error) {
	statePath := d.getStateFilePath(m3u8URL, outputFile)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state ResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (d *Downloader) saveResumeState(state *ResumeState) error {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()

	if err := os.MkdirAll(d.config.ResumeStateDir, 0755); err != nil {
		return err
	}
	statePath := d.getStateFilePath(state.M3U8URL, state.OutputFile)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	// 原子写入：先写临时文件，再重命名
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

func (d *Downloader) cleanResumeState(m3u8URL, outputFile string) error {
	statePath := d.getStateFilePath(m3u8URL, outputFile)
	return os.Remove(statePath)
}

// ==================== 下载核心（支持断点续传、并发、合并） ====================

// Download 同步阻塞下载，支持断点续传和自动合并
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

	// 3. 解析（增强版，给片段加上索引）
	initSegment, segments, segmentKeys, err := d.parsePlaylistEnhancedWithIndex(playlist, baseURL)
	if err != nil {
		result.Error = fmt.Errorf("parse playlist: %w", err)
		return result
	}
	totalSegments := len(segments)
	if totalSegments == 0 {
		result.Error = fmt.Errorf("no segments found")
		return result
	}

	// 4. 确定输出路径
	if outputFile == "" {
		outputFile = d.generateFileName(m3u8URL)
	}
	outputPath := filepath.Join(d.config.OutputDir, outputFile)
	if err := os.MkdirAll(d.config.OutputDir, 0755); err != nil {
		result.Error = fmt.Errorf("mkdir output dir: %w", err)
		return result
	}

	// 5. 断点续传：加载状态
	var resumeState *ResumeState
	if d.config.EnableResume {
		resumeState, err = d.loadResumeState(m3u8URL, outputFile)
		if err != nil {
			d.sendError(fmt.Errorf("load resume state failed: %v, will restart", err))
			resumeState = nil
		}
	}
	// 校验状态是否匹配
	if resumeState != nil && resumeState.TotalSegments != totalSegments {
		d.sendError(fmt.Errorf("segment count mismatch (state:%d, actual:%d), restart", resumeState.TotalSegments, totalSegments))
		resumeState = nil
	}

	// 初始化或重建状态
	if resumeState == nil {
		resumeState = &ResumeState{
			M3U8URL:       m3u8URL,
			OutputFile:    outputFile,
			TotalSegments: totalSegments,
			Completed:     []int{},
			SegmentFiles:  make([]string, totalSegments),
		}
	}

	// 6. 准备临时目录（存放单个片段文件）
	tempDir := filepath.Join(d.config.ResumeStateDir, "segments", filepath.Base(outputFile))
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		result.Error = fmt.Errorf("mkdir temp dir: %w", err)
		return result
	}

	// 7. 处理初始化段（如果有，下载到单独文件，合并时使用）
	initSegmentPath := ""
	if initSegment != nil {
		initSegmentPath = filepath.Join(tempDir, "init.ts")
		if _, err := os.Stat(initSegmentPath); os.IsNotExist(err) {
			data, err := d.downloadSegment(ctx, initSegment.URL)
			if err != nil {
				result.Error = fmt.Errorf("init segment: %w", err)
				return result
			}
			if err := os.WriteFile(initSegmentPath, data, 0644); err != nil {
				result.Error = fmt.Errorf("write init segment: %w", err)
				return result
			}
		}
	}

	// 8. 并发下载未完成的片段
	completedMap := make(map[int]bool)
	for _, idx := range resumeState.Completed {
		completedMap[idx] = true
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, d.config.MaxConcurrent)
	var downloadErr error
	var errMu sync.Mutex
	limiter := &RateLimiter{rate: int64(d.config.RateKB) * 1024}

	for idx, segInfo := range segments {
		if completedMap[idx] {
			continue
		}
		wg.Add(1)
		go func(index int, seg *SegmentInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 片段临时文件路径
			tempFilePath := filepath.Join(tempDir, fmt.Sprintf("segment_%05d.ts", index))
			// 下载片段（支持重试、解密）
			err := d.downloadSegmentToFile(ctx, seg, segmentKeys[index], limiter, tempFilePath)
			if err != nil {
				errMu.Lock()
				if downloadErr == nil {
					downloadErr = fmt.Errorf("segment %d: %w", index, err)
				}
				errMu.Unlock()
				return
			}

			// 更新状态
			resumeState.Completed = append(resumeState.Completed, index)
			resumeState.SegmentFiles[index] = tempFilePath
			if err := d.saveResumeState(resumeState); err != nil {
				d.sendError(fmt.Errorf("save resume state failed: %v", err))
			}

			// 进度回调
			if d.config.OnProgress != nil {
				info, _ := os.Stat(tempFilePath)
				size := int64(0)
				if info != nil {
					size = info.Size()
				}
				d.config.OnProgress(len(resumeState.Completed), totalSegments, index+1, size, 0)
			}
			d.sendError(fmt.Errorf("片段 %d/%d 完成", len(resumeState.Completed), totalSegments))
		}(idx, segInfo)
	}
	wg.Wait()

	if downloadErr != nil {
		result.Error = downloadErr
		return result
	}

	// 9. 所有片段下载完成，进行合并
	var finalOutput string
	if d.config.AutoMerge {
		// 合并为 MP4
		baseName := strings.TrimSuffix(outputPath, filepath.Ext(outputPath))
		finalOutput = baseName + d.config.MergeOutputExt
		if err := d.mergeToMP4(initSegmentPath, resumeState.SegmentFiles, finalOutput); err != nil {
			result.Error = fmt.Errorf("merge to MP4 failed: %w", err)
			return result
		}
	} else {
		// 直接拼接为 TS 文件
		finalOutput = outputPath
		if err := d.concatTSFiles(initSegmentPath, resumeState.SegmentFiles, finalOutput); err != nil {
			result.Error = fmt.Errorf("concat TS failed: %w", err)
			return result
		}
	}

	// 10. 清理临时文件（根据配置）
	if !d.config.KeepSegmentsAfterMerge {
		for _, f := range resumeState.SegmentFiles {
			_ = os.Remove(f)
		}
		if initSegmentPath != "" {
			_ = os.Remove(initSegmentPath)
		}
		_ = os.RemoveAll(tempDir)
	}

	// 11. 清理状态文件
	_ = d.cleanResumeState(m3u8URL, outputFile)

	result.OutputFile = finalOutput
	result.Segments = totalSegments
	result.Size = d.getTotalSize(resumeState.SegmentFiles)
	return result
}

// downloadSegmentToFile 下载单个片段并写入文件（支持重试、解密、限速）
func (d *Downloader) downloadSegmentToFile(ctx context.Context, seg *SegmentInfo, keyInfo *KeyInfo, limiter *RateLimiter, destPath string) error {
	// 如果文件已存在且大小 > 0，直接跳过（片段级续传）
	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		d.sendError(fmt.Errorf("segment file already exists, skip: %s", destPath))
		return nil
	}

	var lastErr error
	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
		if attempt > 0 {
			sleepTime := time.Duration(1<<uint(attempt)) * time.Second
			if sleepTime > 10*time.Second {
				sleepTime = 10 * time.Second
			}
			time.Sleep(sleepTime)
			d.sendError(fmt.Errorf("retry segment %d (attempt %d)", seg.Index, attempt))
		}

		// 下载原始数据
		var data []byte
		var err error
		if seg.Range != "" {
			data, err = d.doRequest(ctx, seg.URL, "bytes="+seg.Range)
		} else {
			data, err = d.doRequest(ctx, seg.URL, "")
		}
		if err != nil {
			lastErr = err
			continue
		}

		// 解密（如果需要）
		if keyInfo != nil {
			key, iv, err := d.prepareKey(ctx, keyInfo, seg.Index)
			if err != nil {
				lastErr = err
				continue
			}
			data, err = d.decryptAES128CBC(data, key, iv)
			if err != nil {
				lastErr = err
				continue
			}
		}

		// 限速
		limiter.Wait(len(data))

		// 写入临时文件
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("failed after %d retries: %w", d.config.MaxRetries, lastErr)
}

// mergeToMP4 使用 ffmpeg 将 TS 片段合并为 MP4
func (d *Downloader) mergeToMP4(initSegmentPath string, segmentFiles []string, outputPath string) error {
	// 创建 concat 文件列表
	listFilePath := filepath.Join(d.config.ResumeStateDir, "concat_list_"+filepath.Base(outputPath)+".txt")
	var listContent strings.Builder
	if initSegmentPath != "" {
		absPath, _ := filepath.Abs(initSegmentPath)
		listContent.WriteString(fmt.Sprintf("file '%s'\n", absPath))
	}
	for _, f := range segmentFiles {
		if f == "" {
			continue
		}
		absPath, _ := filepath.Abs(f)
		listContent.WriteString(fmt.Sprintf("file '%s'\n", absPath))
	}
	if err := os.WriteFile(listFilePath, []byte(listContent.String()), 0644); err != nil {
		return err
	}
	defer os.Remove(listFilePath)

	ffmpegPath := d.config.FfmpegPath
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	cmd := exec.CommandContext(context.Background(), ffmpegPath,
		"-f", "concat",
		"-safe", "0",
		"-i", listFilePath,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		outputPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg error: %v, output: %s", err, output)
	}
	return nil
}

// concatTSFiles 直接拼接 TS 文件（不转码）
func (d *Downloader) concatTSFiles(initSegmentPath string, segmentFiles []string, outputPath string) error {
	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	if initSegmentPath != "" {
		data, err := os.ReadFile(initSegmentPath)
		if err != nil {
			return err
		}
		if _, err := outFile.Write(data); err != nil {
			return err
		}
	}
	for _, f := range segmentFiles {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := outFile.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// getTotalSize 计算所有片段文件的总大小
func (d *Downloader) getTotalSize(files []string) int64 {
	var total int64
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			total += info.Size()
		}
	}
	return total
}

// ==================== 辅助函数（原有函数增强） ====================

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

// decryptAES128CBC 优化了 PKCS#7 填充验证
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

	// 去除 PKCS#7 填充（带验证）
	if len(plaintext) == 0 {
		return plaintext, nil
	}
	paddingLen := int(plaintext[len(plaintext)-1])
	if paddingLen < 1 || paddingLen > aes.BlockSize {
		return nil, fmt.Errorf("invalid padding length: %d", paddingLen)
	}
	// 验证填充内容
	for i := 0; i < paddingLen; i++ {
		if plaintext[len(plaintext)-1-i] != byte(paddingLen) {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return plaintext[:len(plaintext)-paddingLen], nil
}

func (d *Downloader) parseIV(ivStr string) ([]byte, error) {
	ivStr = strings.TrimPrefix(ivStr, "0x")
	if len(ivStr)%2 != 0 {
		ivStr = "0" + ivStr
	}
	return hex.DecodeString(ivStr)
}

// parsePlaylistEnhancedWithIndex 解析 M3U8，为每个片段分配索引
func (d *Downloader) parsePlaylistEnhancedWithIndex(playlist, baseURL string) (*SegmentInfo, []*SegmentInfo, []*KeyInfo, error) {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	var initSegment *SegmentInfo
	var segments []*SegmentInfo
	var segmentKeys []*KeyInfo
	var currentKey *KeyInfo
	segmentIndex := 0

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
					segments = append(segments, &SegmentInfo{URL: segURL, Range: byteRange, Index: segmentIndex})
					segmentKeys = append(segmentKeys, currentKey)
					segmentIndex++
				}
			}
		case strings.HasPrefix(line, "#"):
			// 其他标签忽略
		default:
			segURL := d.resolveURL(line, baseURL)
			segments = append(segments, &SegmentInfo{URL: segURL, Index: segmentIndex})
			segmentKeys = append(segmentKeys, currentKey)
			segmentIndex++
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

// RemuxTS2MP4 简单复制（保留兼容，但推荐使用自动合并）
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
