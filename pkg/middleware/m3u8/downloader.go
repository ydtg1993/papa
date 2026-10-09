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
	"github.com/ydtg1993/papa/v2/internal/msgqueue"
	"github.com/ydtg1993/papa/v2/pkg/middleware"
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
	config *Config
	client *http.Client
	// proxyClients 按代理地址缓存的客户端（逐下载指定出口时用）；proxyMu 保护它。
	proxyMu      sync.Mutex
	proxyClients map[string]*http.Client
	trackQueue   *msgqueue.MsgQueue[any] // 系统消息队列
	keyCache     sync.Map                // 密钥缓存: key -> *cachedKey
	labors       map[string]*labor       // 任务专用锁管理
	laborMu      sync.RWMutex            // 保护 labors map
}

type cachedKey struct {
	key []byte
}

// labor 每个下载任务的私有数据（并发安全）
type labor struct {
	outDir   string
	filename string
	source   string
	resumeMu sync.Mutex // 保护 resumeState 的修改
	execMu   sync.Mutex
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
		trackQueue: msgqueue.NewMsgQueue[any](cfg.QueueSize),
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

// OutputDir 返回配置里的输出根目录（DownloadResult.OutputFile 就是相对它的路径）。
//
// 为什么要有这个 getter：调用方把「相对路径」变成能落库、能在后台目录里看到的本地路径
// （`filepath.Join(下载器.OutputDir(), result.OutputFile)`），这中间只能靠它 —— 若是自己再写一个
// 常量去对齐 `Config.OutputDir`，两处一旦漂了**不报错**，只表现为「文件下下来了但记的路径指空」。
func (d *Downloader) OutputDir() string {
	return d.config.OutputDir
}

// clientFor 返回本次下载该用的 HTTP 客户端：没指定代理就是下载器那个；
// 指定了就用走该代理的客户端（按地址缓存，连接池不白扔）。
func (d *Downloader) clientFor(opts *DownloadOptions) *http.Client {
	if opts == nil || opts.Proxy == "" {
		return d.client
	}
	d.proxyMu.Lock()
	defer d.proxyMu.Unlock()
	if c, ok := d.proxyClients[opts.Proxy]; ok {
		return c
	}
	proxyURL, err := neturl.Parse(opts.Proxy)
	if err != nil {
		d.trackQueue.SendError(fmt.Errorf("parse proxy URL %q: %w", opts.Proxy, err))
		return d.client
	}
	if d.proxyClients == nil {
		d.proxyClients = make(map[string]*http.Client)
	}
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	d.proxyClients[opts.Proxy] = c
	return c
}

// doRequest 执行 HTTP 请求，支持重试
func (d *Downloader) doRequest(ctx context.Context, url, rangeHeader string, opts *DownloadOptions) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= d.config.MaxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if attempt > 0 {
			base := time.Duration(d.config.RetryInterval) * time.Second
			if base == 0 {
				base = 2 * time.Second
			}
			sleepTime := base * time.Duration(1<<uint(attempt-1))
			if sleepTime > 10*time.Second {
				sleepTime = 10 * time.Second
			}
			// 退避要能被 ctx 打断：Engine.Stop 只等 crawler.stop_timeout（模板 5s），
			// 而这里最多睡 10s —— 卡住的话 Stop 会报"未排空"，并连带跳过关库与关浏览器池的收尾
			// （那两件事正是在 drained=false 时故意不做的）。与 engine/stage.go 的退避同一写法。
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(sleepTime):
			}
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
		resp, err := d.clientFor(opts).Do(req)
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

		// 读之前先定一个上界，而不是无脑 ReadAll —— 远端返回超大（或干脆不结束）的响应时，
		// ReadAll 会一路吃内存直到 OOM。上界是**推出来的**，不是拍的魔数：
		//   - 带 Range 的请求：期望长度就写在 Range 里（`bytes=start-end` → `end-start+1`）
		//   - 不带 Range：用响应自己声明的 Content-Length（多给的一律算异常）
		// 两者都推不出来（chunked 又没有 Range）时退回无界读 —— 那种情况没有任何可依据的数字，
		// 宁可保持原样，也不凭空定一个"分片最大多少"（那是替业务做决定）。
		// 注意：**只有上界**。少收字节由 net/http 拿 Content-Length 校验，不用在这儿管。
		limit := int64(-1)
		if rangeHeader != "" {
			if n, ok := rangeLength(rangeHeader); ok {
				limit = n
			}
		}
		if limit < 0 && resp.ContentLength > 0 {
			limit = resp.ContentLength
		}
		data, err := readCapped(resp.Body, limit)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, fmt.Errorf("failed after %d retries: %w", d.config.MaxRetries, lastErr)
}

// rangeLength 从 `bytes=start-end` 解析出这一段期望的字节数；格式不认识时 ok=false。
// 它给 doRequest 提供一个**推出来的**上界，省得为"一个分片最大多少"定一个魔数。
func rangeLength(h string) (int64, bool) {
	s, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, false
	}
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(lo), 10, 64)
	end, err2 := strconv.ParseInt(strings.TrimSpace(hi), 10, 64)
	if err1 != nil || err2 != nil || end < start {
		return 0, false
	}
	return end - start + 1, true
}

// readCapped 读响应体，最多 limit 字节；limit < 0 表示无界。
// 超上界**报错而不是截断** —— 截断会静默产出一个坏分片，那正是这一档最忌讳的失败形态。
func readCapped(r io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return io.ReadAll(r)
	}
	// 多读一个字节：读满 limit+1 就说明对方给的比声明的还多，不用把剩下的全吃进来
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("响应体超过 %d 字节（Range 区间或 Content-Length 声明的长度）", limit)
	}
	return data, nil
}

// SegmentInfo 片段信息
type SegmentInfo struct {
	URL   string
	Range string
	Index int // 0-based 索引
	// MediaSeq 媒体序号 = EXT-X-MEDIA-SEQUENCE + Index。
	// 密钥标签不带 IV 时，HLS 规定用它作为片段的 IV（RFC 8216 §5.2），逐片段不同
	MediaSeq int64
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
	dir, file, ok := middleware.SanitizeOutputPath(outputDir, outputFile)
	if !ok {
		return &DownloadResult{Error: fmt.Errorf("invalid output path: dir=%q file=%q", outputDir, outputFile)}
	}
	outputDir, outputFile = dir, file
	key := m3u8URL + "|" + outputDir + "|" + outputFile
	d.laborMu.Lock()
	l, exists := d.labors[key]
	if !exists {
		l = &labor{
			source:   m3u8URL,
			outDir:   outputDir,
			filename: outputFile,
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
			// 选流结果不进错误通道：那是"这次下载做了什么选择"，不是错误 ——
			// 每下载一个文件写一行 ERROR，正常运行的日志就被这类正常事件填满了。
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
	// 并发上限兜底为 1：这个 channel 是当信号量用的，MaxConcurrent 为 0（或负数）时
	// make(chan struct{}, 0) 是**无缓冲**通道，第一个 goroutine 就会永久阻塞在
	// `sem <- struct{}{}` 上，wg.Wait() 再也不返回 —— 表现为"下载卡死"：
	// 不报错、不退出、也没有任何日志。写 1 至少让它一条条跑完。
	// （同 saveBatchSize 的处理方式：只补"会卡死"的那一类，不动其它字段的语义。）
	sem := make(chan struct{}, max(cfg.MaxConcurrent, 1))
	var downloadErr error
	var errMu sync.Mutex
	limiter := NewRateLimiter(cfg.RateKB)

	saveBatchSize := cfg.SaveBatchSize
	if saveBatchSize <= 0 {
		saveBatchSize = 10
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
		}(idx, segInfo)
	}
	wg.Wait()
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

	relOutput := filepath.Join(outputDir, outputFile)
	if cfg.AutoMerge {
		baseName := strings.TrimSuffix(relOutput, filepath.Ext(relOutput))
		relOutput = baseName + cfg.MergeOutputExt
	}
	result.OutputFile = relOutput
	result.Segments = totalSegments
	result.Size = d.getTotalSize(resumeState.SegmentFiles)
	return result
}

// downloadSegmentToFile 下载单个片段并写入文件（支持重试、解密、限速）
func (d *Downloader) downloadSegmentToFile(ctx context.Context, seg *SegmentInfo, keyInfo *KeyInfo, limiter *RateLimiter, destPath string, opts *DownloadOptions) error {
	// 如果文件已存在且大小 > 0，直接跳过（片段级续传）。
	// 「非空即完整」成立的前提是下面那句写入走"临时文件 + 改名"（见那里的注释）。
	if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
		d.trackQueue.SendError(fmt.Errorf("segment file already exists, skip: %s", destPath))
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
			sleepTime := time.Duration(1<<uint(attempt)) * time.Second
			if sleepTime > 10*time.Second {
				sleepTime = 10 * time.Second
			}
			// 退避要能被 ctx 打断：Engine.Stop 只等 crawler.stop_timeout（模板 5s），
			// 而这里最多睡 10s —— 卡住的话 Stop 会报"未排空"，并连带跳过关库与关浏览器池的收尾
			// （那两件事正是在 drained=false 时故意不做的）。与 engine/stage.go 的退避同一写法。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleepTime):
			}
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
			key, iv, err := d.prepareKey(ctx, keyInfo, seg.MediaSeq, opts)
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

		// 先写临时文件再改名，让 destPath 上只可能出现**完整**的分片。
		// 直接写 destPath 的话，进程被杀或写失败会留下一个非空的半截分片，
		// 而上面那句"已存在且 size>0 就跳过"会把它当成下好的 → 静默合并出损坏的媒体。
		// （短读本身不用在这儿管：doRequest 的 io.ReadAll 配上 net/http 的
		//   Content-Length 校验已经把"少收了几字节"变成了错误。）
		tmpPath := destPath + ".tmp"
		if err := os.WriteFile(tmpPath, data, 0644); err != nil {
			lastErr = err
			continue
		}
		if err := os.Rename(tmpPath, destPath); err != nil {
			_ = os.Remove(tmpPath)
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
	hash := fmt.Sprintf("%x", md5.Sum([]byte(outputPath)))
	listFilePath := filepath.Join(d.config.ResumeStateDir, "concat_list_"+hash+".txt")
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

// prepareKey 取回解密用的密钥与 IV。mediaSeq 是该片段的媒体序号，仅在密钥标签不带 IV 时用于推导 IV。
func (d *Downloader) prepareKey(ctx context.Context, keyInfo *KeyInfo, mediaSeq int64, opts *DownloadOptions) (key, iv []byte, err error) {
	// IV 每次现算，不进缓存：隐式 IV 由媒体序号推导，逐片段不同
	var ivData []byte
	if keyInfo.IV != "" {
		ivData, err = d.parseIV(keyInfo.IV)
		if err != nil {
			return nil, nil, err
		}
	} else {
		ivData = make([]byte, 16)
		binary.BigEndian.PutUint64(ivData[8:], uint64(mediaSeq))
	}
	// 缓存只放密钥本身（同一 URI 的所有片段共用一把），IV 随片段返回
	if cached, ok := d.keyCache.Load(keyInfo.URL); ok {
		return cached.(*cachedKey).key, ivData, nil
	}
	keyData, err := d.doRequest(ctx, keyInfo.URL, "", opts)
	if err != nil {
		return nil, nil, err
	}
	d.keyCache.Store(keyInfo.URL, &cachedKey{key: keyData})
	return keyData, ivData, nil
}

// decryptAES128CBC 解密 AES-128-CBC 数据，自动去除 PKCS#7 填充
func (d *Downloader) decryptAES128CBC(ciphertext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// crypto 边界上的兜底：cipher.NewCBCDecrypter 对非 16 字节的 IV 直接 panic。
	// 今天 IV 只有 prepareKey 一个来源、且已在 parseIV 校验过，这条到不了；
	// 放在这里是因为函数名就承诺「接受任意 iv」，将来多一个调用点时不该把 panic 带进来。
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("invalid IV length: got %d bytes, want %d", len(iv), aes.BlockSize)
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

// parseIV 解析 #EXT-X-KEY 里的显式 IV。
//
// **长度必须在这里挡住**：IV 来自远端播放列表，而下游的 cipher.NewCBCDecrypter 对
// 非 16 字节的 IV 是直接 panic（不是返回 error），且调用发生在下载 goroutine 里、
// 全包没有 recover —— 放过去就是整个进程崩。RFC 8216 要求 IV 恰好 16 字节。
func (d *Downloader) parseIV(ivStr string) ([]byte, error) {
	ivStr = strings.TrimPrefix(ivStr, "0x")
	if len(ivStr)%2 != 0 {
		ivStr = "0" + ivStr
	}
	iv, err := hex.DecodeString(ivStr)
	if err != nil {
		return nil, fmt.Errorf("invalid IV %q: %w", ivStr, err)
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("invalid IV length: got %d bytes, want %d", len(iv), aes.BlockSize)
	}
	return iv, nil
}

// parsePlaylistEnhancedWithIndex 解析 M3U8，为每个片段分配索引
func (d *Downloader) parsePlaylistEnhancedWithIndex(playlist, baseURL string) (*SegmentInfo, []*SegmentInfo, []*KeyInfo, error) {
	scanner := bufio.NewScanner(strings.NewReader(playlist))
	var initSegment *SegmentInfo
	var segments []*SegmentInfo
	var segmentKeys []*KeyInfo
	var currentKey *KeyInfo
	segmentIndex := 0
	var mediaSequence int64

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#EXT-X-VERSION") || strings.HasPrefix(line, "#EXT-X-TARGETDURATION") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64)
			if err != nil {
				// 解析不出来就无法推导隐式 IV，硬报错，避免解出垃圾数据还不自知
				return nil, nil, nil, fmt.Errorf("invalid EXT-X-MEDIA-SEQUENCE %q: %w", line, err)
			}
			mediaSequence = v
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
					segments = append(segments, &SegmentInfo{URL: segURL, Range: byteRange, Index: segmentIndex, MediaSeq: mediaSequence + int64(segmentIndex)})
					segmentKeys = append(segmentKeys, currentKey)
					segmentIndex++
				}
			}
		case strings.HasPrefix(line, "#"):
			// 其他标签忽略
		default:
			segURL := d.resolveURL(line, baseURL)
			segments = append(segments, &SegmentInfo{URL: segURL, Index: segmentIndex, MediaSeq: mediaSequence + int64(segmentIndex)})
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
	base := path.Base(u.Path)
	base = strings.TrimSuffix(base, ".m3u8")
	if base == "" || base == "." || base == "/" || base == "\\" {
		base = fmt.Sprintf("video_%d", time.Now().Unix())
	}
	return base + ".ts"
}
