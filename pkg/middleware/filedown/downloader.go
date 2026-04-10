package filedown

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pkg2 "github.com/ydtg1993/papa/pkg"
)

// Downloader 文件下载器（并发安全）
type Downloader struct {
	config     *Config
	client     *http.Client
	trackQueue *pkg2.MsgQueue[any]
	labors     map[string]*labor
	laborMu    sync.RWMutex
}

// labor 每个下载任务的私有数据
type labor struct {
	url       string
	outputDir string
	filename  string
	execMu    sync.Mutex // 确保同一任务不并发执行
	resumeMu  sync.Mutex // 保护 resumeState 的修改
}

func (l *labor) getUniqueKey() string {
	return l.url + "|" + l.outputDir + "|" + l.filename
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
		labors:     make(map[string]*labor),
	}
}

// SetClient 替换 HTTP 客户端
func (d *Downloader) SetClient(client *http.Client) {
	d.client = client
}

// GetErrors 获取错误通道
func (d *Downloader) GetErrors() <-chan error {
	return d.trackQueue.Errors()
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
	OutputFile string // 相对于 OutputDir 的路径
	Size       int64
	Error      error
}

// ResumeState 断点续传状态
type ResumeState struct {
	URL        string   `json:"url"`
	OutputFile string   `json:"output_file"`
	TotalSize  int64    `json:"total_size"`
	Completed  []int64  `json:"completed"`
	ChunkSize  int64    `json:"chunk_size"`
	TempFiles  []string `json:"temp_files"`
}

// getStateFilePath 获取状态文件路径
func (d *Downloader) getStateFilePath(la *labor) string {
	hash := md5.Sum([]byte(la.url + "|" + la.outputDir + "|" + la.filename))
	hashStr := hex.EncodeToString(hash[:])
	return filepath.Join(d.config.ResumeStateDir, la.outputDir, hashStr+".json")
}

// loadResumeState 加载状态
func (d *Downloader) loadResumeState(la *labor) (*ResumeState, error) {
	fp := d.getStateFilePath(la)
	data, err := os.ReadFile(fp)
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

// saveResumeState 保存状态
func (d *Downloader) saveResumeState(la *labor, state *ResumeState) error {
	la.resumeMu.Lock()
	defer la.resumeMu.Unlock()

	stateDir := filepath.Join(d.config.ResumeStateDir, la.outputDir)
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

// cleanResumeState 清理状态文件
func (d *Downloader) cleanResumeState(la *labor) error {
	statePath := d.getStateFilePath(la)
	return os.Remove(statePath)
}

// Download 同步阻塞下载
func (d *Downloader) Download(ctx context.Context, fileURL, outputDir, fileName string, opts ...*DownloadOptions) *DownloadResult {
	if fileName == "" {
		fileName = d.genFileName(fileURL, "")
	}
	key := fileURL + "|" + outputDir + "|" + fileName
	d.laborMu.Lock()
	l, exists := d.labors[key]
	if !exists {
		l = &labor{
			url:       fileURL,
			outputDir: outputDir,
			filename:  fileName,
		}
		d.labors[key] = l
	}
	d.laborMu.Unlock()

	defer func() {
		d.laborMu.Lock()
		delete(d.labors, key)
		d.laborMu.Unlock()
	}()
	var opt *DownloadOptions
	if len(opts) > 0 && opts[0] != nil {
		opt = opts[0]
	}
	reqCfg := d.buildRequestConfig(opt)
	return d.download(ctx, l, reqCfg)
}

// download 内部实现
func (d *Downloader) download(ctx context.Context, la *labor, cfg *requestConfig) *DownloadResult {
	la.execMu.Lock()
	defer la.execMu.Unlock()

	result := &DownloadResult{}
	cfgGlobal := d.config

	// HEAD 探测
	totalSize, supportRange, contentDisposition := d.headFile(ctx, la.url, cfg)
	if totalSize <= 0 || !supportRange {
		return d.downloadSingle(ctx, la, cfg, contentDisposition)
	}

	relOutput := filepath.Join(la.outputDir, la.filename)
	absOutput := filepath.Join(cfgGlobal.OutputDir, relOutput)
	if err := os.MkdirAll(filepath.Dir(absOutput), 0755); err != nil {
		result.Error = err
		return result
	}

	// 断点续传状态
	var resumeState *ResumeState
	if cfgGlobal.EnableResume {
		var err error
		resumeState, err = d.loadResumeState(la)
		if err != nil {
			d.trackQueue.SendError(fmt.Errorf("load resume state failed: %w", err))
			resumeState = nil
		}
	}
	if resumeState != nil && (resumeState.TotalSize != totalSize || resumeState.ChunkSize != cfgGlobal.ChunkSize) {
		d.trackQueue.SendError(fmt.Errorf("state mismatch, restart"))
		resumeState = nil
	}
	if resumeState == nil {
		resumeState = &ResumeState{
			URL:        la.url,
			OutputFile: la.filename,
			TotalSize:  totalSize,
			ChunkSize:  cfgGlobal.ChunkSize,
			Completed:  []int64{},
			TempFiles:  []string{},
		}
	}
	completedMap := make(map[int64]bool)
	for _, start := range resumeState.Completed {
		completedMap[start] = true
	}

	// 临时目录
	tempDir := filepath.Join(cfgGlobal.ResumeStateDir, "segments", la.outputDir, la.filename)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		result.Error = err
		return result
	}

	// 并发下载
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfgGlobal.MaxConcurrent)
	var downloadErr error
	var errMu sync.Mutex
	var downloaded atomic.Int64
	saveBatchSize := cfgGlobal.SaveBatchSize
	if saveBatchSize <= 0 {
		saveBatchSize = 5
	}
	var completedCount atomic.Int32

	// 分片起始位置列表
	var chunks []int64
	for start := int64(0); start < totalSize; start += cfgGlobal.ChunkSize {
		chunks = append(chunks, start)
	}
	totalChunks := len(chunks)

	for _, start := range chunks {
		if completedMap[start] {
			continue
		}
		wg.Add(1)
		go func(start int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			end := start + cfgGlobal.ChunkSize - 1
			if end >= totalSize {
				end = totalSize - 1
			}
			tempFile := filepath.Join(tempDir, fmt.Sprintf("chunk_%020d.tmp", start))
			err := d.downloadChunkToFile(ctx, la.url, start, end, tempFile, cfg)
			if err != nil {
				errMu.Lock()
				if downloadErr == nil {
					downloadErr = fmt.Errorf("chunk %d: %w", start, err)
				}
				errMu.Unlock()
				return
			}

			la.resumeMu.Lock()
			resumeState.Completed = append(resumeState.Completed, start)
			resumeState.TempFiles = append(resumeState.TempFiles, tempFile)
			la.resumeMu.Unlock()

			newCompleted := completedCount.Add(1)
			chunkSize := end - start + 1
			downloaded.Add(chunkSize)

			if int(newCompleted)%saveBatchSize == 0 || int(newCompleted) == totalChunks {
				if err := d.saveResumeState(la, resumeState); err != nil {
					d.trackQueue.SendError(fmt.Errorf("save resume state failed: %w", err))
				}
			}
			if cfgGlobal.OnProgress != nil {
				cfgGlobal.OnProgress(downloaded.Load(), totalSize)
			}
			d.trackQueue.SendError(fmt.Errorf("分片 %d/%d 完成", newCompleted, totalChunks))
		}(start)
	}
	wg.Wait()
	if downloadErr != nil {
		result.Error = downloadErr
		return result
	}

	// 合并分片（使用 io.NewOffsetWriter 避免大内存）
	outFile, err := os.OpenFile(absOutput, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		result.Error = err
		return result
	}
	defer outFile.Close()
	if err := outFile.Truncate(totalSize); err != nil {
		result.Error = err
		return result
	}
	for _, tempFile := range resumeState.TempFiles {
		// 从文件名解析起始偏移量
		var start int64
		base := filepath.Base(tempFile)
		parts := strings.Split(strings.TrimSuffix(base, ".tmp"), "_")
		if len(parts) == 2 {
			fmt.Sscanf(parts[1], "%020d", &start)
		}
		// 使用 offset writer 流式复制
		src, err := os.Open(tempFile)
		if err != nil {
			result.Error = fmt.Errorf("open temp file %s: %w", tempFile, err)
			return result
		}
		offsetWriter := io.NewOffsetWriter(outFile, start)
		_, err = io.Copy(offsetWriter, src)
		src.Close()
		if err != nil {
			result.Error = fmt.Errorf("write at %d: %w", start, err)
			return result
		}
		if !cfgGlobal.KeepSegmentsAfterMerge {
			_ = os.Remove(tempFile)
		}
	}
	if !cfgGlobal.KeepSegmentsAfterMerge {
		_ = os.RemoveAll(tempDir)
	}
	_ = d.cleanResumeState(la)

	result.OutputFile = relOutput
	result.Size = totalSize
	return result
}

// downloadChunkToFile 下载分片到临时文件
func (d *Downloader) downloadChunkToFile(ctx context.Context, fileURL string, start, end int64, destFile string, cfg *requestConfig) error {
	if info, err := os.Stat(destFile); err == nil && info.Size() == (end-start+1) {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
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
		tmpPath := destFile + ".tmp"
		out, err := os.Create(tmpPath)
		if err != nil {
			_ = resp.Body.Close()
			lastErr = err
			continue
		}
		_, err = io.Copy(out, resp.Body)
		_ = out.Close()
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if err := os.Rename(tmpPath, destFile); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("chunk [%d-%d] failed: %w", start, end, lastErr)
}

// downloadSingle 降级单线程下载
func (d *Downloader) downloadSingle(ctx context.Context, la *labor, cfg *requestConfig, contentDisposition string) *DownloadResult {
	cfgGlobal := d.config
	relOutput := filepath.Join(la.outputDir, la.filename)
	absOutput := filepath.Join(cfgGlobal.OutputDir, relOutput)
	if err := os.MkdirAll(filepath.Dir(absOutput), 0755); err != nil {
		return &DownloadResult{Error: err}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", la.url, nil)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	d.applyHeadersToReq(req, cfg)
	resp, err := d.client.Do(req)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	defer resp.Body.Close()

	file, err := os.Create(absOutput)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	defer file.Close()

	buf := make([]byte, 128*1024)
	var written int64
	total := resp.ContentLength
	if total <= 0 {
		total = -1
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = file.Write(buf[:n])
			written += int64(n)
			if cfgGlobal.OnProgress != nil {
				cfgGlobal.OnProgress(written, total)
			}
		}
		if readErr != nil {
			break
		}
	}
	return &DownloadResult{
		OutputFile: relOutput,
		Size:       written,
	}
}

// headFile 发送 HEAD 请求
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

// applyHeadersToReq 应用请求头
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

// buildRequestConfig 合并配置
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

// genFileName 生成文件名
func (d *Downloader) genFileName(rawURL, contentDisposition string) string {
	parseCD := func(header string) string {
		_, params, err := mime.ParseMediaType(header)
		if err != nil {
			return ""
		}
		if name, ok := params["filename*"]; ok {
			parts := strings.SplitN(name, "'", 3)
			if len(parts) == 3 {
				if decoded, err := url.QueryUnescape(parts[2]); err == nil {
					return decoded
				}
			}
		}
		if name, ok := params["filename"]; ok {
			return name
		}
		return ""
	}
	if contentDisposition != "" {
		if name := parseCD(contentDisposition); name != "" {
			return filepath.Base(name)
		}
	}
	if u, err := url.Parse(rawURL); err == nil {
		base := path.Base(u.Path)
		if base != "" && base != "/" && base != "." {
			if decoded, err := url.QueryUnescape(base); err == nil {
				return filepath.Base(decoded)
			}
		}
	}
	return fmt.Sprintf("file_%d", time.Now().UnixNano())
}
