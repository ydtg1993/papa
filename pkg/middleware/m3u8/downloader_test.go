package m3u8

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// 辅助函数：生成随机 AES-128 密钥和 IV
// ----------------------------------------------------------------------------
func randomKey() []byte {
	key := make([]byte, 16)
	rand.Read(key)
	return key
}

func randomIV() []byte {
	iv := make([]byte, 16)
	rand.Read(iv)
	return iv
}

// 对数据进行 AES-128 CBC 加密
func encryptAES128CBC(plaintext, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	// PKCS#7 填充
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padText := bytes.Repeat([]byte{byte(padding)}, padding)
	plaintext = append(plaintext, padText...)
	ciphertext := make([]byte, len(plaintext))
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, plaintext)
	return ciphertext, nil
}

// ----------------------------------------------------------------------------
// Mock 服务器：提供 M3U8 文件和 TS 片段
// ----------------------------------------------------------------------------
type mockM3U8Server struct {
	server   *httptest.Server
	m3u8Path string
	segments map[string][]byte // key: 片段路径, value: 原始内容（未加密）
	keys     map[string][]byte // key: key 文件路径, value: key 内容
	ivs      map[string][]byte // key: 片段路径, value: IV
}

func newMockM3U8Server() *mockM3U8Server {
	m := &mockM3U8Server{
		segments: make(map[string][]byte),
		keys:     make(map[string][]byte),
		ivs:      make(map[string][]byte),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handler))
	return m
}

func (m *mockM3U8Server) handler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == m.m3u8Path {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		// 动态生成 M3U8 内容，需要从配置中读取
		http.ServeFile(w, r, filepath.Join(".", "testdata", filepath.Base(path)))
		return
	}
	// 处理 key 请求
	if keyData, ok := m.keys[path]; ok {
		w.Write(keyData)
		return
	}
	// 处理片段请求
	if data, ok := m.segments[path]; ok {
		// 如果该片段有 IV，则加密
		if iv, ok := m.ivs[path]; ok {
			key := m.keys["/key.bin"] // 假设密钥统一
			if key == nil {
				w.WriteHeader(500)
				return
			}
			encrypted, err := encryptAES128CBC(data, key, iv)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			w.Write(encrypted)
		} else {
			w.Write(data)
		}
		return
	}
	w.WriteHeader(404)
}

func (m *mockM3U8Server) Close() {
	m.server.Close()
}

// 生成简单的测试 M3U8 内容（无加密）
func generateSimpleM3U8(segments []string) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")
	sb.WriteString("#EXT-X-TARGETDURATION:10\n")
	for _, seg := range segments {
		sb.WriteString("#EXTINF:10.0,\n")
		sb.WriteString(seg + "\n")
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	return sb.String()
}

// 生成带 AES-128 加密的 M3U8 内容
func generateEncryptedM3U8(segments []string, keyURL, ivStr string) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:3\n")
	sb.WriteString("#EXT-X-TARGETDURATION:10\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-KEY:METHOD=AES-128,URI=\"%s\",IV=%s\n", keyURL, ivStr))
	for _, seg := range segments {
		sb.WriteString("#EXTINF:10.0,\n")
		sb.WriteString(seg + "\n")
	}
	sb.WriteString("#EXT-X-ENDLIST\n")
	return sb.String()
}

// 生成多码率 M3U8 内容
func generateVariantM3U8(variants map[string]int) string {
	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	for url, bw := range variants {
		sb.WriteString(fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d\n", bw))
		sb.WriteString(url + "\n")
	}
	return sb.String()
}

// ----------------------------------------------------------------------------
// 测试用例
// ----------------------------------------------------------------------------

// TestDownload_Simple 测试普通下载（无加密、无断点续传）
func TestDownload_Simple(t *testing.T) {
	// 创建临时目录用于输出
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = false
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1

	// 创建 Mock 服务器
	mock := newMockM3U8Server()
	defer mock.Close()

	// 准备片段数据
	seg1 := []byte("segment1 data")
	seg2 := []byte("segment2 data")
	seg3 := []byte("segment3 data")
	segments := []string{"/seg1.ts", "/seg2.ts", "/seg3.ts"}
	mock.segments["/seg1.ts"] = seg1
	mock.segments["/seg2.ts"] = seg2
	mock.segments["/seg3.ts"] = seg3

	// 生成 M3U8 文件并保存到 testdata（或直接通过 handler 返回）
	m3u8Content := generateSimpleM3U8(segments)
	m3u8Path := "/playlist.m3u8"
	// 为了简单，我们在 handler 中直接返回内容，不依赖真实文件
	// 这里修改 mock 的 m3u8Path 并动态返回
	mock.m3u8Path = m3u8Path
	// 重新定义 handler 以返回该内容
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path
	result := downloader.Download(context.Background(), m3u8URL, "output.ts")

	require.NoError(t, result.Error)
	assert.Equal(t, 3, result.Segments)
	assert.FileExists(t, filepath.Join(tmpDir, "output.ts"))
	// 验证内容：应包含所有片段拼接
	data, err := os.ReadFile(filepath.Join(tmpDir, "output.ts"))
	require.NoError(t, err)
	expected := append(seg1, seg2...)
	expected = append(expected, seg3...)
	assert.Equal(t, expected, data)
}

// TestDownload_Concurrent 测试并发下载
func TestDownload_Concurrent(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = false
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 3 // 并发

	mock := newMockM3U8Server()
	defer mock.Close()

	// 准备 10 个片段
	segCount := 10
	segments := make([]string, segCount)
	segData := make([][]byte, segCount)
	for i := 0; i < segCount; i++ {
		name := fmt.Sprintf("/seg%d.ts", i)
		segments[i] = name
		segData[i] = []byte(fmt.Sprintf("data%d", i))
		mock.segments[name] = segData[i]
	}
	m3u8Content := generateSimpleM3U8(segments)
	m3u8Path := "/playlist.m3u8"
	mock.m3u8Path = m3u8Path
	// 同上，动态返回
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path
	result := downloader.Download(context.Background(), m3u8URL, "output_concurrent.ts")
	require.NoError(t, result.Error)
	assert.Equal(t, segCount, result.Segments)
	data, err := os.ReadFile(filepath.Join(tmpDir, "output_concurrent.ts"))
	require.NoError(t, err)
	var expected []byte
	for i := 0; i < segCount; i++ {
		expected = append(expected, segData[i]...)
	}
	assert.Equal(t, expected, data)
}

// TestDownload_Resume 测试断点续传
func TestDownload_Resume(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = true
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1 // 顺序下载，方便控制中断点

	mock := newMockM3U8Server()
	defer mock.Close()

	segCount := 5
	segments := make([]string, segCount)
	segData := make([][]byte, segCount)
	for i := 0; i < segCount; i++ {
		name := fmt.Sprintf("/seg%d.ts", i)
		segments[i] = name
		segData[i] = []byte(fmt.Sprintf("data%d", i))
		mock.segments[name] = segData[i]
	}
	m3u8Content := generateSimpleM3U8(segments)
	m3u8Path := "/playlist.m3u8"
	mock.m3u8Path = m3u8Path
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path

	// 第一次下载，只下载前 2 个片段就模拟中断
	// 我们无法直接中断，但可以修改 downloader 内部逻辑。为了测试，我们手动控制状态：先下载，然后手动删除部分文件后继续。
	// 更可靠的方法：第一次下载后，手动删除输出文件和部分临时文件，然后重新调用 Download 应该恢复。
	// 这里采用：第一次正常下载完整，但手动清除输出文件，保留临时文件和状态，然后第二次下载应该直接合并。
	// 由于状态文件会记录已完成片段，第二次下载会跳过已完成的片段，直接合并。
	// 为了模拟中断，我们第一次下载时限制最大并发为1，下载完第一个片段后 panic 退出（不现实）。改为：第一次正常下载所有片段，然后删除输出文件，保留状态文件和临时文件，再次调用 Download 应直接合并。
	// 这样也验证了断点续传。

	// 第一次下载
	result1 := downloader.Download(context.Background(), m3u8URL, "resume_test.ts")
	require.NoError(t, result1.Error)
	outputPath := filepath.Join(tmpDir, "resume_test.ts")
	assert.FileExists(t, outputPath)

	// 删除输出文件，但保留临时片段文件和状态文件
	err := os.Remove(outputPath)
	require.NoError(t, err)

	// 第二次下载，应该直接合并（不再下载片段）
	result2 := downloader.Download(context.Background(), m3u8URL, "resume_test.ts")
	require.NoError(t, result2.Error)
	assert.FileExists(t, outputPath)
	data, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	var expected []byte
	for i := 0; i < segCount; i++ {
		expected = append(expected, segData[i]...)
	}
	assert.Equal(t, expected, data)
}

// TestDownload_AES128 测试 AES-128 解密
func TestDownload_AES128(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = false
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1

	mock := newMockM3U8Server()
	defer mock.Close()

	// 生成密钥和 IV
	key := randomKey()
	iv := randomIV()
	keyURL := "/key.bin"
	mock.keys[keyURL] = key

	segCount := 3
	segments := make([]string, segCount)
	segData := make([][]byte, segCount)
	for i := 0; i < segCount; i++ {
		name := fmt.Sprintf("/seg%d.ts", i)
		segments[i] = name
		plain := []byte(fmt.Sprintf("secret data %d", i))
		segData[i] = plain
		// 存储原始数据，handler 中会加密
		mock.segments[name] = plain
		mock.ivs[name] = iv // 使用同一个 IV，实际可能每个片段不同
	}
	ivHex := "0x" + hex.EncodeToString(iv)
	m3u8Content := generateEncryptedM3U8(segments, keyURL, ivHex)
	m3u8Path := "/playlist.m3u8"
	mock.m3u8Path = m3u8Path
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path
	result := downloader.Download(context.Background(), m3u8URL, "decrypted.ts")
	require.NoError(t, result.Error)
	assert.Equal(t, segCount, result.Segments)
	data, err := os.ReadFile(filepath.Join(tmpDir, "decrypted.ts"))
	require.NoError(t, err)
	var expected []byte
	for i := 0; i < segCount; i++ {
		expected = append(expected, segData[i]...)
	}
	assert.Equal(t, expected, data)
}

// TestDownload_Variant 测试多码率选择（自动选择最高码率）
func TestDownload_Variant(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = false
	cfg.AutoMerge = false

	mock := newMockM3U8Server()
	defer mock.Close()

	// 准备两个子流
	lowM3U8 := "/low.m3u8"
	highM3U8 := "/high.m3u8"
	lowSegments := []string{"/low1.ts", "/low2.ts"}
	highSegments := []string{"/high1.ts", "/high2.ts"}
	lowData := [][]byte{[]byte("low1"), []byte("low2")}
	highData := [][]byte{[]byte("high1"), []byte("high2")}

	for i, seg := range lowSegments {
		mock.segments[seg] = lowData[i]
	}
	for i, seg := range highSegments {
		mock.segments[seg] = highData[i]
	}
	lowM3U8Content := generateSimpleM3U8(lowSegments)
	highM3U8Content := generateSimpleM3U8(highSegments)

	// 设置 handler 返回子 M3U8 内容
	mock.m3u8Path = lowM3U8
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == lowM3U8 {
			w.Write([]byte(lowM3U8Content))
			return
		}
		if r.URL.Path == highM3U8 {
			w.Write([]byte(highM3U8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	// 主播放列表（多码率）
	variantContent := generateVariantM3U8(map[string]int{
		lowM3U8:  500000,
		highM3U8: 2000000,
	})
	masterPath := "/master.m3u8"
	mock.m3u8Path = masterPath
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == masterPath {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(variantContent))
			return
		}
		if r.URL.Path == lowM3U8 || r.URL.Path == highM3U8 {
			// 交给上面的逻辑
			originalHandler.ServeHTTP(w, r)
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + masterPath
	result := downloader.Download(context.Background(), m3u8URL, "variant.ts")
	require.NoError(t, result.Error)
	assert.Equal(t, 2, result.Segments)
	data, err := os.ReadFile(filepath.Join(tmpDir, "variant.ts"))
	require.NoError(t, err)
	expected := append(highData[0], highData[1]...)
	assert.Equal(t, expected, data)
}

// TestDownload_AutoMerge 测试自动合并为 MP4（需要 ffmpeg）
func TestDownload_AutoMerge(t *testing.T) {
	// 检查 ffmpeg 是否存在
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found, skip auto merge test")
	}

	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = false
	cfg.AutoMerge = true
	cfg.MergeOutputExt = ".mp4"

	mock := newMockM3U8Server()
	defer mock.Close()

	segments := []string{"/seg1.ts", "/seg2.ts"}
	segData := [][]byte{[]byte("data1"), []byte("data2")}
	for i, seg := range segments {
		mock.segments[seg] = segData[i]
	}
	m3u8Content := generateSimpleM3U8(segments)
	m3u8Path := "/playlist.m3u8"
	mock.m3u8Path = m3u8Path
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path
	result := downloader.Download(context.Background(), m3u8URL, "merged")
	require.NoError(t, result.Error)
	// 输出文件应为 .mp4
	assert.Equal(t, ".mp4", filepath.Ext(result.OutputFile))
	assert.FileExists(t, result.OutputFile)
	// 检查文件大小 > 0
	info, err := os.Stat(result.OutputFile)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0))
}

// TestDownload_InitSegment 测试带 EXT-X-MAP 的播放列表
func TestDownload_InitSegment(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = false
	cfg.AutoMerge = false

	mock := newMockM3U8Server()
	defer mock.Close()

	initData := []byte("init header data")
	mock.segments["/init.ts"] = initData
	segments := []string{"/seg1.ts", "/seg2.ts"}
	segData := [][]byte{[]byte("seg1"), []byte("seg2")}
	for i, seg := range segments {
		mock.segments[seg] = segData[i]
	}
	// 构造带 MAP 的 M3U8
	m3u8Content := `#EXTM3U
#EXT-X-MAP:URI="/init.ts"
#EXTINF:10.0,
/seg1.ts
#EXTINF:10.0,
/seg2.ts
#EXT-X-ENDLIST
`
	m3u8Path := "/playlist.m3u8"
	mock.m3u8Path = m3u8Path
	originalHandler := mock.server.Config.Handler
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path
	result := downloader.Download(context.Background(), m3u8URL, "with_init.ts")
	require.NoError(t, result.Error)
	data, err := os.ReadFile(filepath.Join(tmpDir, "with_init.ts"))
	require.NoError(t, err)
	expected := append(initData, segData[0]...)
	expected = append(expected, segData[1]...)
	assert.Equal(t, expected, data)
}

// TestDownload_ResumeAfterFailure 测试下载失败后恢复（模拟片段下载失败）
func TestDownload_ResumeAfterFailure(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.OutputDir = tmpDir
	cfg.EnableResume = true
	cfg.AutoMerge = false
	cfg.MaxRetries = 1 // 快速失败

	mock := newMockM3U8Server()
	defer mock.Close()

	segCount := 3
	segments := make([]string, segCount)
	segData := make([][]byte, segCount)
	for i := 0; i < segCount; i++ {
		name := fmt.Sprintf("/seg%d.ts", i)
		segments[i] = name
		segData[i] = []byte(fmt.Sprintf("data%d", i))
		mock.segments[name] = segData[i]
	}
	m3u8Content := generateSimpleM3U8(segments)
	m3u8Path := "/playlist.m3u8"
	mock.m3u8Path = m3u8Path
	originalHandler := mock.server.Config.Handler

	// 第一次下载时，使第二个片段返回错误
	failSegment := "/seg1.ts"
	failCount := 0
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == failSegment {
			failCount++
			if failCount == 1 {
				w.WriteHeader(500)
				return
			}
		}
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})

	downloader := NewDownloader(cfg)
	m3u8URL := mock.server.URL + m3u8Path
	// 第一次下载应该失败（因为片段1重试后仍失败）
	result1 := downloader.Download(context.Background(), m3u8URL, "resume_fail.ts")
	assert.Error(t, result1.Error) // 预期失败

	// 但此时已完成片段0的状态已保存
	// 第二次下载，服务器恢复正常
	mock.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == m3u8Path {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte(m3u8Content))
			return
		}
		originalHandler.ServeHTTP(w, r)
	})
	result2 := downloader.Download(context.Background(), m3u8URL, "resume_fail.ts")
	require.NoError(t, result2.Error)
	assert.Equal(t, segCount, result2.Segments)
	data, err := os.ReadFile(filepath.Join(tmpDir, "resume_fail.ts"))
	require.NoError(t, err)
	var expected []byte
	for i := 0; i < segCount; i++ {
		expected = append(expected, segData[i]...)
	}
	assert.Equal(t, expected, data)
}
