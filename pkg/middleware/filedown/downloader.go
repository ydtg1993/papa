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

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/msgqueue"
	"github.com/ydtg1993/papa/v3/pkg/middleware"
)

// Downloader 文件下载器（并发安全）
type Downloader struct {
	config *Config
	client *http.Client
	st     *state
}

// state 一份实例的**可变内部状态**。抽出来只有一个目的：让 `With` 造出来的实例能**共用**它 ——
// 于是"随用随生成、配置各不同"与下面两条保证可以同时成立：
//
//   - `labors`：同一个目标文件（键 = 源|目录|文件名）只对应一条 labor，labor 里的 execMu
//     确保**同一任务不并发执行** —— 两条并发下载写同一个文件、同一份断点状态，就靠它挡；
//   - `proxyClients`：每个代理一条连接池。
//
// 想要独立配置不必连这些也独立（用 With）；真要一座互不共享的孤岛，才用 NewDownloader。
type state struct {
	// proxyClients 按代理地址缓存的客户端（逐下载指定出口时用）；proxyMu 保护它。
	proxyMu      sync.Mutex
	proxyClients map[string]*http.Client
	trackQueue   *msgqueue.MsgQueue[any]
	labors       map[string]*labor
	laborMu      sync.RWMutex
}

// labor 每个下载任务的私有数据
type labor struct {
	url       string
	outputDir string
	filename  string
	execMu    sync.Mutex // 确保同一任务不并发执行
	resumeMu  sync.Mutex // 保护 resumeState 的修改
	// waiters 正在使用或排队等这条 labor 的下载数。只在 laborMu 下读写 —— 它是"这条 labor
	// 能不能从表里删"的引用计数，见 acquireLabor / releaseLabor。
	waiters int
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
		st: &state{
			trackQueue: msgqueue.NewMsgQueue[any](cfg.QueueSize),
			labors:     make(map[string]*labor),
		},
	}
}

// SetClient 替换 HTTP 客户端
func (d *Downloader) SetClient(client *http.Client) {
	d.client = client
}

// GetErrors 获取错误通道
// With 在**本实例的配置**之上叠一层，返回一个**新实例**：配置是新的，`labors`/代理连接池/
// 消息队列**共用同一份**（见 state 的注释）—— "随用随生成、配置互不干扰"，但不因此丢掉
// "同一目标文件串行"与连接池复用。原实例一字不改。
//
//	fetcher 里临时一份：dl := engine.FiledownForTask(task).With(filedown.Overlay{OutputDir: &dir})
//
// 想要**完全隔离**的一份用 NewDownloader —— 代价是并发写同一个目标文件时，串行由你自己负责。
func (d *Downloader) With(o Overlay) *Downloader {
	cfg := o.Apply(*d.config)
	return &Downloader{
		config: &cfg,
		client: &http.Client{Timeout: cfg.Timeout},
		st:     d.st,
	}
}

// Config 返回这份实例当前生效的配置（副本；改它不影响实例）。
// 给自查用：`papa check` 与启动日志要把"这一层到底生效了什么"打出来。
func (d *Downloader) Config() Config { return *d.config }

func (d *Downloader) GetErrors() <-chan error {
	return d.st.trackQueue.Errors()
}

// OutputDir 返回配置里的输出根目录（DownloadResult.OutputFile 就是相对它的路径）。
//
// 为什么要有这个 getter：调用方把「相对路径」变成能落库、能在后台目录里看到的本地路径
// （`filepath.Join(下载器.OutputDir(), result.OutputFile)`），这中间只能靠它 —— 若是自己再写一个
// 常量去对齐 `Config.OutputDir`，两处一旦漂了**不报错**，只表现为「文件下下来了但记的路径指空」。
func (d *Downloader) OutputDir() string {
	return d.config.OutputDir
}

// DownloadOptions 单次下载选项
type DownloadOptions struct {
	UserAgent string
	Referer   string
	Cookie    string
	Headers   map[string]string
	// Proxy 本次下载走哪个代理（如 "http://1.2.3.4:8080"）；空 = 用下载器默认的客户端。
	// 出口由调用方决定：`engine.NextProxy()` 取一个，或站点自己的固定出口。
	// 同一个地址的客户端会复用（连接池不白扔）。
	Proxy string
	// Direct 直接下载：跳过 HEAD 探测与分片/续传那套机器，一次 GET 落盘。
	// 封面这类几十~几百 KB 的小文件用它可以省掉一次往返（也就不会再有"分片 1/1"这种过程）。
	// 代价：拿不到 HEAD 上的 Content-Disposition 与 Content-Length，也没有断点续传
	//（大文件、会断的下载还是走默认的分片路径）。
	Direct bool
}

// OptionsFromRequest 按这次任务的抓取上下文造一份下载选项：站点级 + 逐请求 headers、
// 显式指定的代理都会带过来；referer 是下载要声明的来源页（封面/视频这类资源站点常查它防盗链）。
//
// 为什么要有它：抓取与下载打的是同一个站，往往要同一套头（UA / Cookie / Referer 少一个就被拒），
// 但引擎的抓取上下文（站点 `SiteSpec.Headers`、`papa.WithHeaders`、`papa.WithProxyURL`）与
// `DownloadOptions` 是两套东西，而**下载器不会自己去读 ctx** —— 隐式继承会让人不知道请求上到底带了什么。
// 所以是显式的一步：想用就调它，不想用就自己造一份。
//
//	opts := filedown.OptionsFromRequest(ctx, page.URL.String())
//	opts.Direct = true
//	res := engine.GetFiledown().Download(ctx, coverURL, "covers/series", name, opts)
//
// 返回新对象，改它不影响 ctx。ctx 里既没有头也没有代理时，就是一份只带 Referer 的选项。
// 头里的 `User-Agent`、`Cookie` 直接以 header 形式带过去；ctx 里若有 `Referer` 而这里也传了
// referer，以**参数为准**（`applyHeadersToReq` 最后写 Referer）。
//
// `Headers` **总是可写**（没头时是空 map，不是 nil）：调用方常要再补自己的键
// （`opts.Headers["Accept"] = "image/webp"`），给个 nil map 就是等着 panic。
//
// **它只带 ctx 上那两层**：站点级（`SiteSpec.Headers`，框架在任务入口挂上）与逐请求
// （`papa.WithHeaders`）。config.yaml 里全局的 `html.headers` / `browser.headers` **不在其中** ——
// 那是抓取客户端的默认层（还要支持热更），这里读不到。要让下载也跟抓取同一套头（UA 尤其），
// 把该键写进 `SiteSpec.Headers`（站点声明，两条路都继承得到），或自己补在这个 map 上。
func OptionsFromRequest(ctx context.Context, referer string) *DownloadOptions {
	headers := core.HeadersFrom(ctx) // 返回的已经是副本；没有时是 nil
	if headers == nil {
		headers = map[string]string{}
	}
	// 空值在抓取那边是"删掉这个头"（见 core.ApplyHeaders / splitHeaderOverride），
	// 而下载器里 Set 一个空值会真的发出去一个空头（服务器往往据此判为异常请求）——
	// 所以把空值就地删掉，让两边的语义一致。
	for k, v := range headers {
		if v == "" {
			delete(headers, k)
		}
	}
	return &DownloadOptions{
		Referer: referer,
		Proxy:   core.ProxyURLFrom(ctx),
		Headers: headers,
	}
}

// DownloadResult 下载结果
type DownloadResult struct {
	OutputFile string // 相对于 OutputDir 的路径
	Size       int64
	// ContentType 响应声明的类型（分片下载取自 HEAD 探测，单线程下载取自那一次 GET）。
	//
	// 为什么要带出来：文件名得调用方自己从 URL 猜，而站点常用图片代理
	//（`/image?url=<编码后的原图>`），它按 `Accept` 协商输出格式 —— 源图是 webp、
	// 只声明 `image/*` 就返回 jpeg，照 URL 后缀存下来就是"后缀与内容不符"。
	// 拿这个值（或下载后按文件头嗅探）复核一下，再决定落盘的后缀。
	ContentType string
	Error       error
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
// acquireLabor 取（或建）这一任务的 labor，并把引用数 +1。
//
// 引用数保证一条不变量：**只要还有人排队，这条 labor 就不从表里删**。于是同键的后来者排在同一条
// labor 的 execMu 后面，绝不会各自拿到一条新 labor 并发写同一份产物 —— 老写法是"用完无条件 delete"，
// A 结束删掉条目后、B 还排在旧 labor 的锁上，此时 C 又能建一条新的，B/C 就各拿一条锁并发写同一个文件。
func (d *Downloader) acquireLabor(key, url, outputDir, fileName string) *labor {
	d.st.laborMu.Lock()
	defer d.st.laborMu.Unlock()
	l, ok := d.st.labors[key]
	if !ok {
		l = &labor{url: url, outputDir: outputDir, filename: fileName}
		d.st.labors[key] = l
	}
	l.waiters++
	return l
}

// releaseLabor 引用数 -1，归零才删。**先 -1 再判断**：引用数是"还有人要用这条 labor"的计数，
// 归零前删掉就等于把后来者的锁抽走（就是无条件 delete 那个老 bug）。
func (d *Downloader) releaseLabor(key string, l *labor) {
	d.st.laborMu.Lock()
	defer d.st.laborMu.Unlock()
	l.waiters--
	if l.waiters == 0 {
		delete(d.st.labors, key)
	}
}

// segDir 分片临时目录。
//
// **必须带源**：状态文件按 `md5(源|目录|文件名)` 区分，分片目录也得按源区分，否则"换源重写到同一个
// 输出路径"（同一路径先下 A 源、再下 B 源）会复用上一源的旧分片 —— 分片跳过只看磁盘大小、不认源，
// 于是拼出混合内容还返回成功。哈希用源 URL 即可（目录与文件名已经字面量进路径了）。
//
// 续传身份 = **源 URL + 输出路径 + 大小/分块大小**（见 download 里对 TotalSize/ChunkSize 的比对）。
// 同一个 URL 的内容**变了但长度不变**时这里分不出来（要 ETag/Last-Modified，Range 请求不保证给），
// 属爬虫框架的既定边界：要抓新内容，换 URL 或换输出文件名。
func (d *Downloader) segDir(la *labor) string {
	h := md5.Sum([]byte(la.url))
	return filepath.Join(d.config.ResumeStateDir, "segments", la.outputDir, la.filename+"-"+hex.EncodeToString(h[:]))
}

func (d *Downloader) cleanResumeState(la *labor) error {
	statePath := d.getStateFilePath(la)
	return os.Remove(statePath)
}

// Download 同步阻塞下载
func (d *Downloader) Download(ctx context.Context, fileURL, outputDir, fileName string, opts ...*DownloadOptions) *DownloadResult {
	if fileName == "" {
		fileName = d.genFileName(fileURL, "")
	}
	dir, file, ok := middleware.SanitizeOutputPath(outputDir, fileName)
	if !ok {
		return &DownloadResult{Error: fmt.Errorf("invalid output path: dir=%q file=%q", outputDir, fileName)}
	}
	outputDir, fileName = dir, file
	key := fileURL + "|" + outputDir + "|" + fileName
	l := d.acquireLabor(key, fileURL, outputDir, fileName)
	defer d.releaseLabor(key, l)
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

	cfgGlobal := d.config

	// 分块大小走兜底：<= 0 见 chunkSizeOrDefault 的说明（那是个"会卡死"的值）
	chunkSize := chunkSizeOrDefault(cfgGlobal.ChunkSize)

	// 直接下载（`DownloadOptions.Direct`）：不探测、不分片。给封面这类小文件用 ——
	// 省掉一次 HEAD 往返，也就不必为"一个 GET 就够"的文件去走分片+续传那套机器。
	if cfg.direct {
		return d.downloadSingle(ctx, la, cfg, probe{})
	}

	// HEAD 探测
	p := d.headFile(ctx, la.url, cfg)
	if p.TotalSize <= 0 || !p.SupportRange {
		return d.downloadSingle(ctx, la, cfg, p)
	}

	result := &DownloadResult{ContentType: p.ContentType}
	totalSize := p.TotalSize

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
			d.st.trackQueue.SendError(fmt.Errorf("load resume state failed: %w", err))
			resumeState = nil
		}
	}
	if resumeState != nil && (resumeState.TotalSize != totalSize || resumeState.ChunkSize != chunkSize) {
		d.st.trackQueue.SendError(fmt.Errorf("state mismatch, restart"))
		resumeState = nil
	}
	if resumeState == nil {
		resumeState = &ResumeState{
			URL:        la.url,
			OutputFile: la.filename,
			TotalSize:  totalSize,
			ChunkSize:  chunkSize,
			Completed:  []int64{},
			TempFiles:  []string{},
		}
	}
	// 状态文件里"已完成的分片"只当**索引**用，不当事实：偏移必须落在分片边界上才算数。
	// 不信的话，损坏/被改过的状态里任何一个数字都能成为"往输出文件某个位置写字节"的许可证
	//（合并那一步就是按这个偏移定位写的）。边界之外的当它没下过 —— 于是那一片会被重新下载。
	completedMap := make(map[int64]bool, len(resumeState.Completed))
	for _, start := range resumeState.Completed {
		if !isChunkStart(start, totalSize, chunkSize) {
			d.st.trackQueue.SendError(fmt.Errorf("resume state: 忽略非法分片偏移 %d（不在分片边界上），该片按未下载处理", start))
			continue
		}
		completedMap[start] = true
	}

	// 临时目录（按源隔离，见 segDir）
	tempDir := d.segDir(la)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		result.Error = err
		return result
	}

	// 并发下载
	var wg sync.WaitGroup
	// 并发上限兜底为 1：这个 channel 是当信号量用的，MaxConcurrent 为 0（或负数）时
	// make(chan struct{}, 0) 是**无缓冲**通道，第一个 goroutine 就会永久阻塞在
	// `sem <- struct{}{}` 上，wg.Wait() 再也不返回 —— 表现为"下载卡死"：
	// 不报错、不退出、也没有任何日志。负数则直接 panic。
	// 写 1 至少让它一块块串行跑完。（同下面 saveBatchSize 的处理方式：
	// 只补"会卡死/崩"的那一类，不动"分块大小"这种业务语义字段。）
	sem := make(chan struct{}, max(cfgGlobal.MaxConcurrent, 1))
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
	for start := int64(0); start < totalSize; start += chunkSize {
		chunks = append(chunks, start)
	}
	totalChunks := len(chunks)

	for _, start := range chunks {
		// 照状态文件跳过下载 —— 但**合并那一步会按偏移+长度复核一遍**（见下），
		// 状态说"下好了"而磁盘上没有/长度不对时，那里会清掉状态并报错，让下一次跑重下这一片。
		if completedMap[start] {
			continue
		}
		wg.Add(1)
		go func(start int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			end := start + chunkSize - 1
			if end >= totalSize {
				end = totalSize - 1
			}
			tempFile := chunkTempPath(tempDir, start)
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
					d.st.trackQueue.SendError(fmt.Errorf("save resume state failed: %w", err))
				}
			}
			// 进度只走 OnProgress，别改回错误通道：那端（App.mdMsgListener）按 Error 级别落盘，
			// 拿它传进度等于把"成功"写成 ERROR —— 下 11 张封面就是 11 行 level=error，
			// 把真错误稀释掉，而且业务侧关不掉（只要用 filedown 就有）。
			if cfgGlobal.OnProgress != nil {
				cfgGlobal.OnProgress(downloaded.Load(), totalSize)
			}
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
	// 遍历的**不是** `resumeState.TempFiles` 里的路径，而是"已完成分片"的偏移：路径一律由偏移拼出来。
	// 状态文件是本应用自己写的，但损坏或被改过时，照它给的路径 os.Open 就能把进程读得到的任意文件
	// 合并进下载产物；照它给的偏移写，就是往产物的任意位置塞字节。`TempFiles` 仍照写进状态文件
	// （格式不动），只是不再被当成路径来源。
	for _, start := range resumeState.Completed {
		if !isChunkStart(start, totalSize, chunkSize) {
			d.st.trackQueue.SendError(fmt.Errorf("resume state: 合并时忽略非法分片偏移 %d（不在分片边界上）", start))
			continue
		}
		tempFile := chunkTempPath(tempDir, start)
		// 使用 offset writer 流式复制
		src, err := os.Open(tempFile)
		if err != nil {
			// 状态文件说这个分片下好了，文件却不在 —— 只可能是状态与磁盘对不上
			// （进程在"删分片"与"清状态"之间被杀，见下面收尾处的注释）。
			// 顺手把状态文件清掉：下一次跑从头重下，而不是每次都栽在这一步、
			// 永久失败到只能人工去删状态文件。
			_ = d.cleanResumeState(la)
			result.Error = fmt.Errorf("open temp file %s: %w（已清掉断点状态，下次重跑会重新下载）", tempFile, err)
			return result
		}
		// 长度也要对得上：短分片会让这一片的目标区间留下零填充空洞（产物先 Truncate(totalSize)）。
		// 这与"文件不在"同一处理 —— 清状态、报错，让下一次跑重下，而不是交出一个看着成功的坏文件。
		if info, serr := src.Stat(); serr != nil || info.Size() != chunkLength(start, totalSize, chunkSize) {
			_ = src.Close()
			_ = d.cleanResumeState(la)
			result.Error = fmt.Errorf("segment %d: 临时分片 %s 长度不符（应有 %d 字节，已清掉断点状态，下次重跑会重新下载）",
				start, tempFile, chunkLength(start, totalSize, chunkSize))
			return result
		}
		offsetWriter := io.NewOffsetWriter(outFile, start)
		_, err = io.Copy(offsetWriter, src)
		src.Close()
		if err != nil {
			result.Error = fmt.Errorf("write at %d: %w", start, err)
			return result
		}
		// 注意：这里**不要**顺手删已合并的分片。
		// 合并中途失败时，状态文件仍然写着"所有分片都下好了"，续传会跳过下载直接进合并 ——
		// 要是这轮已经把前几个分片删了，那次合并就会报 open temp file ... no such file。
		// 分片统一由下面的 RemoveAll(tempDir) 在**合并全部成功之后**清掉。
	}

	// 收尾：**先清状态文件，再删分片 —— 顺序不能反。**
	// 反过来（先删分片、后清状态）中间崩一次，留下的就是"状态说全下好了、分片已经删了"：
	// 下次跑跳过所有下载直接进合并 → 撞 open temp file ... no such file →
	// 失败在清理那一步之前返回 → 状态文件还在 → 这个任务**永久失败**，
	// 只能人工去删 <ResumeStateDir>/<outputDir>/<hash>.json。
	// 按现在这个顺序，最坏是"状态没了、分片还在"：重下一遍，多花点带宽，不会卡死。
	_ = d.cleanResumeState(la)
	if !cfgGlobal.KeepSegmentsAfterMerge {
		_ = os.RemoveAll(tempDir)
	}

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
			// 退避要能被 ctx 打断：Engine.Stop 只等 crawler.stop_timeout（模板 5s），
			// 而这里最多睡 10s —— 卡住的话 Stop 会报"未排空"，并连带跳过关库与关浏览器池的收尾
			// （那两件事正是在 drained=false 时故意不做的）。与 engine/stage.go 的退避同一写法。
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(sleepTime):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		d.applyHeadersToReq(req, cfg)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := d.clientOf(cfg).Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusPartialContent {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("unexpected status: %d", resp.StatusCode)
			continue
		}
		expectSize := end - start + 1
		tmpPath := destFile + ".tmp"
		out, err := os.Create(tmpPath)
		if err != nil {
			_ = resp.Body.Close()
			lastErr = err
			continue
		}
		written, err := io.Copy(out, resp.Body)
		_ = resp.Body.Close()
		if err == nil && written != expectSize {
			// 短 206：服务端只回了部分字节，当失败重试，否则合并出空洞文件
			err = fmt.Errorf("short body: got %d bytes, want %d", written, expectSize)
		}
		if cerr := out.Close(); err == nil {
			err = cerr // 磁盘写失败只在 Close 时暴露
		}
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
func (d *Downloader) downloadSingle(ctx context.Context, la *labor, cfg *requestConfig, p probe) *DownloadResult {
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
	resp, err := d.clientOf(cfg).Do(req)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &DownloadResult{Error: fmt.Errorf("unexpected status: %d", resp.StatusCode)}
	}

	// 先写临时文件、成功后整体改名：直接 os.Create(absOutput) 会**先截断**目标 —— 中途任何失败
	//（读错 / 写错 / 磁盘满 / 长度不符）都留下半截文件，还把上一份好文件一起毁掉。
	// 分片路径的每片本来就是"临时文件 + 改名"，这里对齐。
	tmpPath := absOutput + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return &DownloadResult{Error: err}
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()

	buf := make([]byte, 128*1024)
	var written int64
	total := resp.ContentLength
	if total <= 0 {
		total = -1
	}
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := file.Write(buf[:n]); werr != nil {
				return &DownloadResult{Error: fmt.Errorf("write %s: %w", absOutput, werr)}
			}
			written += int64(n)
			if cfgGlobal.OnProgress != nil {
				cfgGlobal.OnProgress(written, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return &DownloadResult{Error: fmt.Errorf("read body: %w", readErr)}
		}
	}
	// 正常路径在此显式关闭以拿到落盘错误（磁盘满/NFS 回写失败）；defer 那次 Close 只兜提前返回。
	if err := file.Close(); err != nil {
		return &DownloadResult{Error: fmt.Errorf("close %s: %w", absOutput, err)}
	}
	if total > 0 && written != total {
		return &DownloadResult{Error: fmt.Errorf("truncated download: got %d bytes, want %d", written, total)}
	}
	if err := os.Rename(tmpPath, absOutput); err != nil {
		return &DownloadResult{Error: fmt.Errorf("finalize %s: %w", absOutput, err)}
	}
	success = true
	// 这条路上真正拿到字节的是这次 GET，所以优先用它声明的类型；
	// GET 没给（少见）才回落到 HEAD 那份 —— 同一个 URL、同一套请求头，问出来的东西一样。
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = p.ContentType
	}
	return &DownloadResult{
		OutputFile:  relOutput,
		Size:        written,
		ContentType: contentType,
	}
}

// probe 是 HEAD 探测的结果：下载前先问一次大小、是否支持 Range、以及响应声明的类型。
type probe struct {
	TotalSize          int64
	SupportRange       bool
	ContentDisposition string
	ContentType        string
}

// headFile 发送 HEAD 请求
func (d *Downloader) headFile(ctx context.Context, fileURL string, cfg *requestConfig) probe {
	req, err := http.NewRequestWithContext(ctx, "HEAD", fileURL, nil)
	if err != nil {
		return probe{}
	}
	d.applyHeadersToReq(req, cfg)
	resp, err := d.clientOf(cfg).Do(req)
	if err != nil || resp.StatusCode >= 400 {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return probe{}
	}
	defer resp.Body.Close()
	return probe{
		TotalSize:          resp.ContentLength,
		SupportRange:       strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes"),
		ContentDisposition: resp.Header.Get("Content-Disposition"),
		ContentType:        resp.Header.Get("Content-Type"),
	}
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
		client:  d.client,
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
	if opts.Proxy != "" {
		cfg.client = d.clientForProxy(opts.Proxy)
	}
	cfg.direct = opts.Direct
	return cfg
}

// clientOf 取本次下载该用的客户端：配置里解析过就用它，否则回落到下载器默认那个。
// 收成一处是为了让"手拼的 requestConfig"（测试、或包内新调用点漏了 buildRequestConfig）
// 不至于在 d.client.Do 上崩掉。
func (d *Downloader) clientOf(cfg *requestConfig) *http.Client {
	if cfg != nil && cfg.client != nil {
		return cfg.client
	}
	return d.client
}

// clientForProxy 返回走指定代理的 HTTP 客户端；同一个地址复用同一个（连接池不白扔）。
//
// 代理地址来自调用方（fetcher），所以数量由业务决定 —— 这里不做上限，也不清理。
// 地址解析不出来时返回默认客户端并记一条错误：让这次下载走直连总比整个失败好，
// 但错误通道上留痕（"代理配错了"不该静默变成"直连"）。
func (d *Downloader) clientForProxy(addr string) *http.Client {
	d.st.proxyMu.Lock()
	defer d.st.proxyMu.Unlock()
	if c, ok := d.st.proxyClients[addr]; ok {
		return c
	}
	proxyURL, err := url.Parse(addr)
	if err != nil {
		d.st.trackQueue.SendError(fmt.Errorf("parse proxy URL %q: %w", addr, err))
		return d.client
	}
	if d.st.proxyClients == nil {
		d.st.proxyClients = make(map[string]*http.Client)
	}
	// 克隆 DefaultTransport 再挂代理：直接 `&http.Transport{}` 会把连接池上限、空闲超时那套
	// 调优全丢掉（每个代理一份全新 Transport，等于每次调用重做 TCP/TLS 握手）。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	c := &http.Client{
		Timeout:   d.config.Timeout,
		Transport: transport,
	}
	d.st.proxyClients[addr] = c
	return c
}

// requestConfig 单次请求配置
type requestConfig struct {
	userAgent string
	referer   string
	cookie    string
	headers   map[string]string
	direct    bool
	// client 本次下载用的 HTTP 客户端：默认是下载器那个，带 Proxy 时换成走该代理的
	//（按地址缓存，见 clientForProxy）。
	client *http.Client
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

// chunkSizeOrDefault 分块大小的兜底：<= 0 会让分片循环**永不退出**（`start += 0` 原地打转、
// `start += 负数` 往回走），表现是进程卡在 OOM 上、连一行日志都没有 —— 与 MaxConcurrent /
// saveBatchSize 同一处理方式：只补"会卡死"的那一类。回落到默认 5MB 而不是 1：
// 1 字节一块会把分片数打成"文件有多少字节"，goroutine 与临时文件的数量同样能把机器拖死。
func chunkSizeOrDefault(v int64) int64 {
	if v <= 0 {
		return DefaultConfig().ChunkSize
	}
	return v
}

// isChunkStart 判断一个偏移是不是合法的分片起点：落在 [0, totalSize) 且是 chunkSize 的整数倍。
//
// 分片偏移一律由这个函数把关（状态文件里的 `Completed` 两条路都用它）—— 合并是按偏移定位写的，
// 认下一个边界之外的值就是往产物任意位置塞字节，而分片循环也不会为它生成任何一格。
func isChunkStart(start, totalSize, chunkSize int64) bool {
	return chunkSize > 0 && start >= 0 && start < totalSize && start%chunkSize == 0
}

// chunkTempPath 分片临时文件路径。**由偏移拼出来**，不从状态文件里读 —— 见合并处的注释。
func chunkTempPath(tempDir string, start int64) string {
	return filepath.Join(tempDir, fmt.Sprintf("chunk_%020d.tmp", start))
}

// chunkLength 第 start 片应有的字节数（最后一片可能不足一个 chunkSize）。
func chunkLength(start, totalSize, chunkSize int64) int64 {
	if n := totalSize - start; n < chunkSize {
		return n
	}
	return chunkSize
}
