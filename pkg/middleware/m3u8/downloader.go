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
	pkg2 "github.com/ydtg1993/papa/pkg"
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
	"sync/atomic"
	"time"
)

// Downloader 全局下载器（线程安全）
type Downloader struct {
	config     *Config
	client     *http.Client
	trackQueue *pkg2.MsgQueue[any] // 系统消息队列
	keyCache   sync.Map            // 密钥缓存: key -> *cachedKey
	labors     map[string]*labor   // 任务专用锁管理
	laborMu    sync.RWMutex        // 保护 labors map
}

type cachedKey struct {
	key []byte
	iv  []byte
}

// labor 每个下载任务的私有数据（并发安全）
type labor struct {
	outDir   string
	filename string
	source   string
	resumeMu sync.Mutex // 保护 resumeState 的修改
	execMu   sync.Mutex
}

func (l *labor) GetUniqueKey() string {
	return l.source + "|" + l.outDir + "|" + l.filename
}

// NewDownloader 创建下载器（全局共享实例）
func NewDownloader(cfg *Config) *Downloader {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &Downloader{
		config: cfg,
		client: &http.Client{
			Timeout: cfg.SegmentTimeout,
		},
		trackQueue: pkg2.NewMsgQueue[any](10),
		keyCache:   sync.Map{},
		labors:     make(map[string]*labor),
	}
}

// SetClient 允许外部替换 HTTP 客户端（可选）
func (d *Downloader) SetClient(client *http.Client) {
	d.client = client
}

// GetErrors 获取错误通道（全局）
func (d *Downloader) GetErrors() <-chan error {
	return d.trackQueue.Errors()
}

// doRequest 执行 HTTP 请求，支持重试
func (d *Downloader) doRequest(ctx context.Context, url, rangeHeader string, opts *DownloadOptions) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
		if attempt > 0 {
			base := time.Duration(d.config.RetryInterval) * time.Second
			if base == 0 {
				base = 2 * time.Second
			}
			sleepTime := base * time.Duration(1<<uint(attempt-1))
			if sleepTime > 10*time.Second {
				sleepTime = 10 * time.Second
			}
			time.Sleep(sleepTime)
			d.trackQueue.SendError(fmt.Errorf("重试 %s (第 %d 次)", url, attempt))
		}

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			lastErr = err
			continue
		}

		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		// 应用请求级配置
		if opts != nil {
			if opts.UserAgent != "" {
				req.Header.Set("User-Agent", opts.UserAgent)
			}
			if opts.Referer != "" {
				req.Header.Set("Referer", opts.Referer)
			}
			if opts.Cookie != "" {
				req.Header.Set("Cookie", opts.Cookie)
			}
			for k, v := range opts.Headers {
				req.Header.Set(k, v)
			}
		}
		// 防止服务器返回压缩数据导致解密失败
		req.Header.Set("Accept-Encoding", "identity")
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

// SegmentInfo 片段信息
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

// getStateFilePath 使用 labor 中的信息
func (d *Downloader) getStateFilePath(la *labor) string {
	hash := fmt.Sprintf("%x", md5.Sum([]byte(la.source+"|"+la.outDir+"|"+la.filename)))
	return filepath.Join(d.config.ResumeStateDir, la.outDir, hash+".json")
}

// loadResumeState 接收 labor
func (d *Downloader) loadResumeState(la *labor) (*ResumeState, error) {
	statePath := d.getStateFilePath(la)
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

// saveResumeState 接收 labor 和 state，使用 labor 中的锁
func (d *Downloader) saveResumeState(la *labor, state *ResumeState) error {
	la.resumeMu.Lock()
	defer la.resumeMu.Unlock()

	stateDir := filepath.Join(d.config.ResumeStateDir, la.outDir)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return err
	}
	statePath := d.getStateFilePath(la)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := statePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, statePath)
}

// cleanResumeState 接收 labor
func (d *Downloader) cleanResumeState(la *labor) error {
	statePath := d.getStateFilePath(la)
	return os.Remove(statePath)
}

// ==================== 下载核心（支持断点续传、并发、合并） ====================

// Download 同步阻塞下载，支持断点续传和自动合并
func (d *Downloader) Download(ctx context.Context, m3u8URL, outputDir, outputFile string, opts ...*DownloadOptions) *DownloadResult {
	if outputFile == "" {
		outputFile = d.generateFileName(m3u8URL)
	}
	l := &labor{
		source:   m3u8URL,
		outDir:   outputDir,
		filename: outputFile,
	}
	d.laborMu.Lock()
	if _, exists := d.labors[l.GetUniqueKey()]; !exists {
		d.labors[l.GetUniqueKey()] = l
	} else {
		l = d.labors[l.GetUniqueKey()]
	}
	d.laborMu.Unlock()

	var opt *DownloadOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
	}
	return d.download(ctx, l, opt)
}

func (d *Downloader) download(ctx context.Context, la *labor, opts *DownloadOptions) *DownloadResult {
	la.execMu.Lock()
	defer la.execMu.Unlock()
	result := &DownloadResult{}
	cfg := d.config
	// 从 labor 中获取任务参数
	m3u8URL := la.source
	outputDir := la.outDir
	outputFile := la.filename

	// 1. 获取播放列表
	playlist, baseURL, err := d.fetchPlaylist(ctx, m3u8URL, opts)
	if err != nil {
		result.Error = fmt.Errorf("fetch playlist: %w", err)
		return result
	}

	// 2. 处理多码率
	if strings.Contains(playlist, "#EXT-X-STREAM-INF") {
		bestURL, err := d.selectBestStream(playlist, baseURL)
		if err == nil {
			d.trackQueue.SendError(fmt.Errorf("选择最高码率流: %s", bestURL))
			playlist, baseURL, err = d.fetchPlaylist(ctx, bestURL, opts)
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
	outputPath := filepath.Join(cfg.OutputDir, outputDir, outputFile)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		result.Error = fmt.Errorf("mkdir output dir: %w", err)
		return result
	}

	// 5. 断点续传：加载状态
	var resumeState *ResumeState
	if cfg.EnableResume {
		resumeState, err = d.loadResumeState(la)
		if err != nil {
			d.trackQueue.SendError(fmt.Errorf("load resume state failed: %w, will restart", err))
			resumeState = nil
		}
	}
	// 校验状态是否匹配
	if resumeState != nil && resumeState.TotalSegments != totalSegments {
		d.trackQueue.SendError(fmt.Errorf("segment count mismatch (state:%d, actual:%d), restart", resumeState.TotalSegments, totalSegments))
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
	tempDir := filepath.Join(cfg.ResumeStateDir, "segments", outputDir, filepath.Base(outputFile))
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		result.Error = fmt.Errorf("mkdir temp dir: %w", err)
		return result
	}

	// 7. 处理初始化段（如果有，下载到单独文件，合并时使用）
	initSegmentPath := ""
	if initSegment != nil {
		initSegmentPath = filepath.Join(tempDir, "init.ts")
		if _, err := os.Stat(initSegmentPath); os.IsNotExist(err) {
			data, err := d.downloadSegment(ctx, initSegment.URL, opts)
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
	sem := make(chan struct{}, cfg.MaxConcurrent)
	var downloadErr error
	var errMu sync.Mutex
	limiter := NewRateLimiter(cfg.RateKB)

	saveBatchSize := cfg.SaveBatchSize
	if saveBatchSize <= 0 {
		saveBatchSize = 1 // 或跳过批量逻辑，每片都保存
	}
	var completedCount int32
	var totalBytes int64

	for idx, segInfo := range segments {
		if completedMap[idx] {
			continue
		}
		wg.Add(1)
		go func(index int, seg *SegmentInfo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			tempFilePath := filepath.Join(tempDir, fmt.Sprintf("segment_%05d.ts", index))
			err := d.downloadSegmentToFile(ctx, seg, segmentKeys[index], limiter, tempFilePath, opts)
			if err != nil {
				errMu.Lock()
				if downloadErr == nil {
					downloadErr = fmt.Errorf("segment %d: %w", index, err)
				}
				errMu.Unlock()
				return
			}

			info, _ := os.Stat(tempFilePath)
			segSize := int64(0)
			if info != nil {
				segSize = info.Size()
			}

			// 使用 labor 中的 resumeMu
			la.resumeMu.Lock()
			resumeState.Completed = append(resumeState.Completed, index)
			resumeState.SegmentFiles[index] = tempFilePath
			la.resumeMu.Unlock()

			newCompleted := atomic.AddInt32(&completedCount, 1)
			atomic.AddInt64(&totalBytes, segSize)

			// 每 saveBatchSize 个片段保存一次状态
			if int(newCompleted)%saveBatchSize == 0 || newCompleted == int32(totalSegments) {
				if err := d.saveResumeState(la, resumeState); err != nil {
					d.trackQueue.SendError(fmt.Errorf("save resume state failed: %w", err))
				}
			}

			// 进度回调
			if cfg.OnProgress != nil {
				cfg.OnProgress(int(newCompleted), totalSegments, index+1, segSize, atomic.LoadInt64(&totalBytes))
			}
			d.trackQueue.SendError(fmt.Errorf("片段 %d/%d 完成", newCompleted, totalSegments))
		}(idx, segInfo)
	}
	wg.Wait()
	// 确保最后再保存一次（防止漏掉不足 saveBatchSize 的部分）
	if err := d.saveResumeState(la, resumeState); err != nil {
		d.trackQueue.SendError(fmt.Errorf("final save resume state failed: %w", err))
	}
	if downloadErr != nil {
		result.Error = downloadErr
		return result
	}

	// 9. 所有片段下载完成，进行合并
	var finalOutput string
	if cfg.AutoMerge {
		baseName := strings.TrimSuffix(outputPath, filepath.Ext(outputPath))
		finalOutput = baseName + cfg.MergeOutputExt
		if err := os.MkdirAll(filepath.Dir(finalOutput), 0755); err != nil {
			result.Error = fmt.Errorf("mkdir final output dir: %w", err)
			return result
		}
		if err := d.mergeToMP4(ctx, initSegmentPath, resumeState.SegmentFiles, finalOutput); err != nil {
			result.Error = fmt.Errorf("merge to MP4 failed: %w", err)
			return result
		}
	} else {
		finalOutput = outputPath
		if err := d.concatTSFiles(initSegmentPath, resumeState.SegmentFiles, finalOutput); err != nil {
			result.Error = fmt.Errorf("concat TS failed: %w", err)
			return result
		}
	}

	// 10. 清理临时文件（根据配置）
	if !cfg.KeepSegmentsAfterMerge {
		for _, f := range resumeState.SegmentFiles {
			_ = os.Remove(f)
		}
		if initSegmentPath != "" {
			_ = os.Remove(initSegmentPath)
		}
		_ = os.RemoveAll(tempDir)
	}

	// 11. 清理状态文件
	_ = d.cleanResumeState(la)

	result.OutputFile = finalOutput
	result.Segments = totalSegments
	result.Size = d.getTotalSize(resumeState.SegmentFiles)

	// 函数末尾添加清理（注意需要知道 key）
	d.laborMu.Lock()
	delete(d.labors, la.GetUniqueKey())
	d.laborMu.Unlock()
	return result
}

// downloadSegmentToFile 下载单个片段并写入文件（支持重试、解密、限速）
func (d *Downloader) downloadSegmentToFile(ctx context.Context, seg *SegmentInfo, keyInfo *KeyInfo, limiter *RateLimiter, destPath string, opts *DownloadOptions) error {
	// 如果文件已存在且大小 > 0，直接跳过（片段级续传）
	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		d.trackQueue.SendError(fmt.Errorf("segment file already exists, skip: %s", destPath))
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
			d.trackQueue.SendError(fmt.Errorf("retry segment %d (attempt %d)", seg.Index, attempt))
		}

		// 下载原始数据
		var data []byte
		var err error
		if seg.Range != "" {
			data, err = d.doRequest(ctx, seg.URL, "bytes="+seg.Range, opts)
		} else {
			data, err = d.doRequest(ctx, seg.URL, "", opts)
		}
		if err != nil {
			lastErr = err
			continue
		}

		// 解密（如果需要）
		if keyInfo != nil {
			key, iv, err := d.prepareKey(ctx, keyInfo, seg.Index, opts)
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
		if err := limiter.Wait(ctx, len(data)); err != nil {
			return err
		}

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
func (d *Downloader) mergeToMP4(ctx context.Context, initSegmentPath string, segmentFiles []string, outputPath string) error {
	// 创建 concat 文件列表
	safeName := strings.ReplaceAll(outputPath, string(filepath.Separator), "_")
	listFilePath := filepath.Join(d.config.ResumeStateDir, "concat_list_"+safeName+".txt")
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
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-f", "concat",
		"-safe", "0",
		"-i", listFilePath,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		outputPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg error: %w, output: %s", err, output)
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

	copyFile := func(srcPath string) error {
		src, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(outFile, src)
		return err
	}

	if initSegmentPath != "" {
		if err := copyFile(initSegmentPath); err != nil {
			return err
		}
	}
	for _, f := range segmentFiles {
		if f == "" {
			continue
		}
		if err := copyFile(f); err != nil {
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
func (d *Downloader) fetchPlaylist(ctx context.Context, m3u8URL string, opts *DownloadOptions) (string, string, error) {
	data, err := d.doRequest(ctx, m3u8URL, "", opts)
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

func (d *Downloader) downloadSegment(ctx context.Context, url string, opts *DownloadOptions) ([]byte, error) {
	return d.doRequest(ctx, url, "", opts)
}

func (d *Downloader) prepareKey(ctx context.Context, keyInfo *KeyInfo, segmentIndex int, opts *DownloadOptions) (key, iv []byte, err error) {
	cacheKey := keyInfo.URL + "|" + keyInfo.IV
	if cached, ok := d.keyCache.Load(cacheKey); ok {
		k := cached.(*cachedKey)
		return k.key, k.iv, nil
	}
	keyData, err := d.doRequest(ctx, keyInfo.URL, "", opts)
	if err != nil {
		return nil, nil, err
	}
	var ivData []byte
	if keyInfo.IV != "" {
		ivData, err = d.parseIV(keyInfo.IV)
		if err != nil {
			return nil, nil, err
		}
	} else {
		ivData = make([]byte, 16)
		binary.BigEndian.PutUint64(ivData[8:], uint64(segmentIndex))
	}
	d.keyCache.Store(keyInfo.URL+"|"+keyInfo.IV, &cachedKey{key: keyData, iv: ivData})
	return keyData, ivData, nil
}

// decryptAES128CBC 解密 AES-128-CBC 数据，自动去除 PKCS#7 填充
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
