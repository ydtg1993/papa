package m3u8

import (
	"time"
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

	// 断点续传相关
	ResumeStateDir         string // 状态文件存储目录，默认 "./downloads/.resume"
	AutoMerge              bool   // 自动合并为 MP4，默认 true
	MergeOutputExt         string // 合并后的扩展名，默认 ".mp4"
	KeepSegmentsAfterMerge bool   // 合并后是否保留原始 TS 文件，默认 false
	FfmpegPath             string // ffmpeg 可执行文件路径，留空则自动查找 PATH
}

// DownloadOptions 单次下载的请求级配置
type DownloadOptions struct {
	UserAgent string            // 自定义 User-Agent，为空则使用 Go 默认
	Referer   string            // 自定义 Referer
	Cookie    string            // 自定义 Cookie
	Headers   map[string]string // 额外 Headers
}

// DefaultConfig 返回默认配置（适合大多数场景）
func DefaultConfig() *Config {
	return &Config{
		OutputDir:      "./downloads",
		MaxConcurrent:  5,
		SegmentTimeout: 30 * time.Second,
		MaxRetries:     3,
		RetryInterval:  2,
		EnableResume:   true,
		RateKB:         0,
		ResumeStateDir: "./downloads/.resume",
		AutoMerge:      true,
		MergeOutputExt: ".mp4",
	}
}
