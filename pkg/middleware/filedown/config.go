package filedown

import (
	"time"
)

// Config 文件下载器配置
type Config struct {
	OutputDir     string                        // 输出目录，默认 "./downloads"
	MaxRetries    int                           // 重试次数，默认 3
	RetryInterval int                           // 重试间隔（秒），默认 1
	Timeout       time.Duration                 // 下载超时，默认 30s
	MaxConcurrent int                           // 并发分片数，默认 4
	ChunkSize     int64                         // 每块大小（字节），默认 5MB
	OnProgress    func(downloaded, total int64) // 进度回调
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		OutputDir:     "./downloads",
		Timeout:       30 * time.Second,
		MaxRetries:    3,
		RetryInterval: 1,
		MaxConcurrent: 4,
		ChunkSize:     5 << 20, // 5MB
	}
}
