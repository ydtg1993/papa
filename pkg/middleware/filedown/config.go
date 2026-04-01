package filedown

import "github.com/sirupsen/logrus"

// Config 文件下载器配置
type Config struct {
	// 输出目录，默认 "./downloads"
	OutputDir string
	// 重试次数，默认 3
	MaxRetries int
	// 重试间隔（秒），默认 2
	RetryInterval int
	// 下载超时（秒），默认 30
	Timeout int
	// 是否启用断点续传（仅对支持 Range 的服务器有效），默认 false
	EnableResume bool
	// 并发数
	MaxConcurrent int
	// 每块大小（如 1MB）
	ChunkSize int64
	// User-Agent
	UserAgent string
	// 自定义请求头
	Headers map[string]string
	// 日志记录器（可选）
	Logger *logrus.Logger
	// 进度回调函数（可选），参数：已下载字节数，总字节数（可能为 -1 表示未知）
	OnProgress func(downloaded, total int64)
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		OutputDir:     "./downloads",
		Timeout:       60,
		MaxRetries:    3,
		RetryInterval: 1,
		EnableResume:  false,
		MaxConcurrent: 4,
		ChunkSize:     1 << 20, // 1MB
		UserAgent:     "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		Headers: map[string]string{
			"Accept":          "*/*",
			"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
			"Connection":      "keep-alive",
		},
	}
}
