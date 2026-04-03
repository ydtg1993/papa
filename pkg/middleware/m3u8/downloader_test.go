package m3u8

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 公开测试用的 M3U8 地址（Apple 官方示例）
var testM3U8URLs = []string{
	"https://devstreaming-cdn.apple.com/videos/streaming/examples/bipbop_4x3/bipbop_4x3_variant.m3u8",
	"https://devstreaming-cdn.apple.com/videos/streaming/examples/bipbop_16x9/bipbop_16x9_variant.m3u8",
	"https://test-streams.mux.dev/x36xhzz/x36xhzz.m3u8",
}

// 检查网络连通性，如果无法访问则跳过测试
func skipIfNoInternet(t *testing.T, url string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Head(url)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("Skipping test because %s is not reachable: %v", url, err)
	}
	if resp != nil {
		resp.Body.Close()
	}
}

// 测试完整下载流程（使用真实 M3U8）
func TestDownloadRealM3U8(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real network test in short mode")
	}

	for _, m3u8URL := range testM3U8URLs {
		t.Run(m3u8URL, func(t *testing.T) {
			skipIfNoInternet(t, m3u8URL)

			cfg := DefaultConfig()
			cfg.MaxConcurrent = 3
			cfg.SegmentTimeout = 10 * time.Second / 2
			cfg.MaxRetries = 1
			cfg.EnableResume = false
			cfg.Logger = defaultLogger()
			downloader := NewDownloader(cfg)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			result := downloader.Download(ctx, m3u8URL, "")
			require.NoError(t, result.Error)
			assert.Greater(t, result.Segments, 0)
			assert.Greater(t, result.Size, int64(0))

			// 验证输出文件存在且非空
			info, err := os.Stat(result.OutputFile)
			require.NoError(t, err)
			assert.Greater(t, info.Size(), int64(0))

			t.Logf("Downloaded %s: %d segments, %d bytes", result.OutputFile, result.Segments, result.Size)
		})
	}
}

// 测试带进度回调的下载
func TestDownloadWithProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real network test in short mode")
	}

	url := testM3U8URLs[0]
	skipIfNoInternet(t, url)

	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.MaxConcurrent = 3
	cfg.SegmentTimeout = 10 * time.Second
	cfg.MaxRetries = 1
	cfg.EnableResume = false
	cfg.Logger = defaultLogger()

	var progressLog []string
	cfg.OnProgress = func(downloadedSegments, totalSegments int, currentSegment int, segmentSize, totalBytes int64) {
		progressLog = append(progressLog, fmt.Sprintf("Progress: %d/%d, total %d bytes", downloadedSegments, totalSegments, totalBytes))
	}

	downloader := NewDownloader(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result := downloader.Download(ctx, url, "")
	require.NoError(t, result.Error)
	assert.NotEmpty(t, progressLog)
	t.Logf("Progress callbacks: %d", len(progressLog))
}

// 测试解析功能（无需网络）
func TestParsePlaylistEnhanced_Local(t *testing.T) {
	d := &Downloader{}

	tests := []struct {
		name     string
		playlist string
		baseURL  string
		wantInit bool
		wantSegs int
		wantKeys int
	}{
		{
			name: "普通无加密",
			playlist: `#EXTM3U
#EXTINF:10,
segment1.ts
#EXTINF:10,
segment2.ts
#EXT-X-ENDLIST`,
			baseURL:  "https://example.com/",
			wantInit: false,
			wantSegs: 2,
			wantKeys: 0,
		},
		{
			name: "带密钥",
			playlist: `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1234567890abcdef1234567890abcdef
#EXTINF:10,
segment1.ts
#EXTINF:10,
segment2.ts`,
			baseURL:  "https://example.com/",
			wantInit: false,
			wantSegs: 2,
			wantKeys: 2,
		},
		{
			name: "带初始化段",
			playlist: `#EXTM3U
#EXT-X-MAP:URI="init.ts"
#EXTINF:10,
segment1.ts
#EXTINF:10,
segment2.ts`,
			baseURL:  "https://example.com/",
			wantInit: true,
			wantSegs: 2,
			wantKeys: 0,
		},
		{
			name: "带 BYTERANGE",
			playlist: `#EXTM3U
#EXT-X-BYTERANGE:1000@0
segment.ts
#EXT-X-BYTERANGE:2000@1000
segment.ts`,
			baseURL:  "https://example.com/",
			wantInit: false,
			wantSegs: 2,
			wantKeys: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initSeg, segs, keys, err := d.parsePlaylistEnhanced(tt.playlist, tt.baseURL)
			assert.NoError(t, err)
			if tt.wantInit {
				assert.NotNil(t, initSeg)
				assert.Equal(t, "https://example.com/init.ts", initSeg.URL)
			} else {
				assert.Nil(t, initSeg)
			}
			assert.Len(t, segs, tt.wantSegs)
			if tt.wantKeys > 0 {
				assert.Len(t, keys, tt.wantSegs)
				for _, k := range keys {
					assert.NotNil(t, k)
				}
			} else {
				for _, k := range keys {
					assert.Nil(t, k)
				}
			}
		})
	}
}

// 测试码率选择（无网络）
func TestSelectBestStream_Local(t *testing.T) {
	d := &Downloader{}
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=500000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000
medium.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000
high.m3u8`
	baseURL := "https://example.com/"
	best, err := d.selectBestStream(playlist, baseURL)
	assert.NoError(t, err)
	assert.Equal(t, "https://example.com/high.m3u8", best)
}

// 测试文件名生成（无网络）
func TestGenerateFileName_Local(t *testing.T) {
	d := &Downloader{}
	tests := []struct {
		url      string
		expected string
	}{
		{"https://example.com/video.m3u8", "video.ts"},
		{"https://example.com/path/to/playlist.m3u8", "playlist.ts"},
		{"https://example.com/noext", "noext.ts"},
		{"invalid-url", "video_*.ts"}, // 匹配时间戳，使用前缀
	}
	for _, tt := range tests {
		name := d.generateFileName(tt.url)
		if tt.expected == "video_*.ts" {
			assert.True(t, strings.HasPrefix(name, "video_") && strings.HasSuffix(name, ".ts"))
		} else {
			assert.Equal(t, tt.expected, name)
		}
	}
}

// 测试限速器（无网络）
func TestRateLimiter_Local(t *testing.T) {
	limiter := &RateLimiter{rate: 1024} // 1KB/s
	start := time.Now()
	limiter.Wait(1024)
	elapsed := time.Since(start)
	assert.True(t, elapsed >= time.Second && elapsed < time.Second+50*time.Millisecond)
}

// 辅助函数：创建测试用的日志记录器（避免输出）
func defaultLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return logger
}
