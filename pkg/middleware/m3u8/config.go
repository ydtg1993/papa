package m3u8

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v2/core"
)

// Config M3U8 下载器全局配置
type Config struct {
	// 输出目录，默认 "./downloads"
	OutputDir string
	// 最大并发下载片段数，默认 5
	MaxConcurrent int
	// 单个片段下载超时，默认 30s
	SegmentTimeout time.Duration
	// 最大重试次数，默认 3
	MaxRetries int
	// 初始重试间隔（秒），实际使用指数退避，默认 2
	RetryInterval int
	// 是否启用断点续传，默认 true
	EnableResume bool
	// 限速（KB/s），0 表示不限速，默认 0
	RateKB int
	// 进度回调函数（可选）
	// 参数：已下载片段数，总片段数，当前片段索引（从1开始），当前片段大小（字节），累计已下载字节数
	OnProgress func(downloadedSegments, totalSegments int, currentSegment int, segmentSize, totalBytes int64)
	// 并发下载保存数量
	SaveBatchSize int
	// 断点续传相关
	ResumeStateDir         string // 状态文件存储目录，默认 "./downloads/.resume"
	AutoMerge              bool   // 自动合并为 MP4，默认 true
	MergeOutputExt         string // 合并后的扩展名，默认 ".mp4"
	KeepSegmentsAfterMerge bool   // 合并后是否保留原始 TS 文件，默认 false
	FfmpegPath             string // ffmpeg 可执行文件路径，留空则自动查找 PATH
	// 系统消息队列（活动/错误上报）容量，默认 100
	QueueSize int
}

// DownloadOptions 单次下载的请求级配置
type DownloadOptions struct {
	UserAgent string            // 自定义 User-Agent，为空则使用 Go 默认
	Referer   string            // 自定义 Referer
	Cookie    string            // 自定义 Cookie
	Headers   map[string]string // 额外 Headers
	// Proxy 本次下载走哪个代理（如 "http://1.2.3.4:8080"）；空 = 用下载器默认的客户端。
	// 出口由调用方决定：`engine.NextProxy()` 取一个，或站点自己的固定出口。
	Proxy string
}

// OptionsFromRequest 按这次任务的抓取上下文造一份下载选项：站点级 + 逐请求 headers、
// 显式指定的代理都会带过来；referer 是下载要声明的来源页（多数 m3u8 站点校验它）。
//
// 与 `filedown.OptionsFromRequest` 同一个约定与理由：抓取上下文（站点 `SiteSpec.Headers`、
// `papa.WithHeaders`、`papa.WithProxyURL`）与 `DownloadOptions` 是两套东西，下载器**不读 ctx** ——
// 隐式继承会让人不知道请求上到底带了什么，所以是显式的一步。
//
//	opts := m3u8.OptionsFromRequest(ctx, page.URL.String())
//	res := engine.GetM3U8().Download(ctx, m3u8URL, dir, file, opts)
//
// 返回新对象，改它不影响 ctx。ctx 里既没有头也没有代理时，就是一份只带 Referer 的选项。
//
// `Headers` **总是可写**（没头时是空 map，不是 nil）：调用方常要再补自己的键，给个 nil map 就是等着 panic。
//
// **它只带 ctx 上那两层**（站点级 `SiteSpec.Headers`、逐请求 `papa.WithHeaders`）；
// config.yaml 里全局的 `html.headers` / `browser.headers` 不在其中（抓取客户端的默认层，这里读不到）
// —— 要让下载跟抓取同一套头，把该键写进 `SiteSpec.Headers`，或自己补在这个 map 上。
func OptionsFromRequest(ctx context.Context, referer string) *DownloadOptions {
	headers := core.HeadersFrom(ctx) // 返回的已经是副本；没有时是 nil
	if headers == nil {
		headers = map[string]string{}
	}
	// 空值在抓取那边是"删掉这个头"（见 core.ApplyHeaders），而下载器里 Set 一个空值会真的
	// 发出去一个空头 —— 就地删掉，让两边语义一致。
	for k, v := range headers {
		if v == "" {
			delete(headers, k)
		}
	}
	if referer != "" {
		// 分片请求上是**先写 Referer 字段、后写这个 map**（见 downloadSegmentToFile），
		// 所以 ctx 里那个 Referer 会盖掉参数。来源页该是详情页，这里显式让参数说了算，
		// 与 filedown.OptionsFromRequest 的行为保持一致。
		delete(headers, "Referer")
	}
	return &DownloadOptions{
		Referer: referer,
		Proxy:   core.ProxyURLFrom(ctx),
		Headers: headers,
	}
}

// DefaultConfig 返回默认配置（适合大多数场景）
func DefaultConfig() *Config {
	return &Config{
		OutputDir:      "./downloads",
		MaxConcurrent:  5,
		SegmentTimeout: 30 * time.Second,
		MaxRetries:     3,
		RetryInterval:  2,
		SaveBatchSize:  10,
		EnableResume:   true,
		RateKB:         0,
		ResumeStateDir: "./downloads/.resume",
		AutoMerge:      true,
		MergeOutputExt: ".mp4",
		QueueSize:      100,
	}
}
