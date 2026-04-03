package m3u8

import (
	"time"
)

// Config M3U8 下载器配置
type Config struct {
	// 输出目录，默认 "./downloads"
	OutputDir string
	// 最大并发下载片段数，默认 5
	MaxConcurrent int
	// 单个片段下载超时（秒），默认 30
	SegmentTimeout time.Duration `yaml:"segment_timeout"`
	// 最大重试次数，默认 3
	MaxRetries int
	// 初始重试间隔（秒），实际使用指数退避，此值作为基础间隔，默认 2
	RetryInterval int
	// 是否启用断点续传，默认 true
	EnableResume bool
	// 限速（KB/s），0 表示不限速，默认 0
	RateKB int
	// User-Agent，默认模拟 Chrome
	UserAgent string
	// 自定义请求头
	Headers map[string]string
	// 进度回调函数（可选）
	// 参数：已下载片段数，总片段数，当前片段索引（从1开始），当前片段大小（字节），累计已下载字节数
	OnProgress func(downloadedSegments, totalSegments int, currentSegment int, segmentSize, totalBytes int64)

	// ========== 新增：断点续传相关 ==========
	// 状态文件存储目录，默认 "./downloads/.resume"
	ResumeStateDir string `yaml:"resume_state_dir"`
	// 自动合并为 MP4，默认 true
	AutoMerge bool `yaml:"auto_merge"`
	// 合并后的扩展名，默认 ".mp4"
	MergeOutputExt string `yaml:"merge_output_ext"`
	// 合并后是否保留原始 TS 文件，默认 false
	KeepSegmentsAfterMerge bool `yaml:"keep_segments_after_merge"`
	// ffmpeg 可执行文件路径，留空则自动查找 PATH
	FfmpegPath string `yaml:"ffmpeg_path"`
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
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		Headers: map[string]string{
			"Accept":          "*/*",
			"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
			"Connection":      "keep-alive",
		},
		// 新增字段默认值
		ResumeStateDir:         "./downloads/.resume",
		AutoMerge:              true,
		MergeOutputExt:         ".mp4",
		KeepSegmentsAfterMerge: false,
		FfmpegPath:             "",
	}
}
