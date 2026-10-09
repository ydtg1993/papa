package m3u8

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/core"
)

// 测试辅助：创建临时目录并自动清理
func tempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "m3u8_test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// 测试 generateFileName
func TestGenerateFileName(t *testing.T) {
	d := &Downloader{}
	tests := []struct {
		url      string
		expected string
	}{
		{"https://example.com/video.m3u8", "video.ts"},
		{"https://example.com/path/playlist.m3u8", "playlist.ts"},
		{"https://example.com/video", "video.ts"},
		{"https://example.com/", "video_.ts"}, // 会包含时间戳，仅检查前缀
	}
	for _, tt := range tests {
		name := d.generateFileName(tt.url)
		if tt.expected == "video_.ts" {
			if !strings.HasPrefix(name, "video_") || !strings.HasSuffix(name, ".ts") {
				t.Errorf("generateFileName(%q) = %q, want prefix 'video_' and suffix '.ts'", tt.url, name)
			}
		} else {
			if name != tt.expected {
				t.Errorf("generateFileName(%q) = %q, want %q", tt.url, name, tt.expected)
			}
		}
	}
}

// 测试 resolveURL
func TestResolveURL(t *testing.T) {
	d := &Downloader{}
	tests := []struct {
		raw, base, expected string
	}{
		{"segment.ts", "https://example.com/path/", "https://example.com/path/segment.ts"},
		{"/segment.ts", "https://example.com/path/", "https://example.com/segment.ts"},
		{"https://other.com/seg.ts", "https://example.com/", "https://other.com/seg.ts"},
		{"segment.ts", "", "segment.ts"},
	}
	for _, tt := range tests {
		got := d.resolveURL(tt.raw, tt.base)
		if got != tt.expected {
			t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.raw, tt.base, got, tt.expected)
		}
	}
}

// 测试解析播放列表（带索引）
func TestParsePlaylistEnhancedWithIndex(t *testing.T) {
	d := &Downloader{}
	playlist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MAP:URI="init.ts"
#EXTINF:5,
segment1.ts
#EXTINF:5,
segment2.ts
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1234567890abcdef1234567890abcdef
#EXTINF:5,
segment3.ts
#EXT-X-BYTERANGE:1024@0
segment4.ts
`
	baseURL := "https://example.com/path/"
	init, segs, keys, err := d.parsePlaylistEnhancedWithIndex(playlist, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if init == nil || init.URL != "https://example.com/path/init.ts" {
		t.Errorf("init segment wrong: %+v", init)
	}
	if len(segs) != 4 {
		t.Fatalf("expected 4 segments, got %d", len(segs))
	}
	// 检查 segment1
	if segs[0].URL != "https://example.com/path/segment1.ts" || segs[0].Index != 0 {
		t.Errorf("segment1: %+v", segs[0])
	}
	// 检查 segment2
	if segs[1].URL != "https://example.com/path/segment2.ts" || segs[1].Index != 1 {
		t.Errorf("segment2: %+v", segs[1])
	}
	// 检查 segment3 带密钥
	if keys[2] == nil || keys[2].URL != "https://example.com/path/key.bin" || keys[2].IV != "0x1234567890abcdef1234567890abcdef" {
		t.Errorf("segment3 key: %+v", keys[2])
	}
	// 检查 byte range
	if segs[3].Range != "1024@0" {
		t.Errorf("segment4 range: %s", segs[3].Range)
	}
}

// 测试多码率流选择
func TestSelectBestStream(t *testing.T) {
	d := &Downloader{}
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1280000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000
mid.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5120000
high.m3u8
`
	baseURL := "https://example.com/"
	best, err := d.selectBestStream(playlist, baseURL)
	if err != nil {
		t.Fatal(err)
	}
	if best != "https://example.com/high.m3u8" {
		t.Errorf("best stream = %q, want high.m3u8", best)
	}
}

// 测试完整下载流程（不依赖 ffmpeg，使用 concatTSFiles）
func TestDownloadIntegration(t *testing.T) {
	// 创建模拟 HTTP 服务器
	mux := http.NewServeMux()
	// 主播放列表
	m3u8Content := `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:1.0,
segment1.ts
#EXTINF:1.0,
segment2.ts
`
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte(m3u8Content))
	})
	// 片段内容
	seg1 := []byte("SEGMENT1 DATA")
	seg2 := []byte("SEGMENT2 DATA")
	mux.HandleFunc("/segment1.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(seg1)
	})
	mux.HandleFunc("/segment2.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(seg2)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// 创建临时目录
	outDir := tempDir(t)
	stateDir := tempDir(t)

	cfg := DefaultConfig()
	cfg.OutputDir = outDir
	cfg.ResumeStateDir = stateDir
	cfg.AutoMerge = false // 避免调用 ffmpeg
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.SaveBatchSize = 1
	downloader := NewDownloader(cfg)

	url := server.URL + "/playlist.m3u8"
	outputFile := "test_output.ts"
	outputDirRel := "subdir"

	result := downloader.Download(context.Background(), url, outputDirRel, outputFile, nil)
	if result.Error != nil {
		t.Fatalf("download failed: %s", result.Error.Error())
	}
	if result.Segments != 2 {
		t.Errorf("Segments = %d, want 2", result.Segments)
	}
	expectedRel := filepath.Join(outputDirRel, outputFile)
	if result.OutputFile != expectedRel {
		t.Errorf("OutputFile = %q, want %q", result.OutputFile, expectedRel)
	}
	// 检查输出文件内容
	absPath := filepath.Join(outDir, expectedRel)
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedData := append(seg1, seg2...)
	if string(data) != string(expectedData) {
		t.Errorf("file content mismatch: got %q, want %q", data, expectedData)
	}
	// 检查状态文件是否已清理
	stateFiles, _ := filepath.Glob(filepath.Join(stateDir, outputDirRel, "*.json"))
	if len(stateFiles) != 0 {
		t.Errorf("state file not cleaned: %+v", stateFiles)
	}
}

// 测试断点续传：取消下载后恢复
func TestDownloadResume(t *testing.T) {
	// 模拟服务器，每个片段响应稍慢
	mux := http.NewServeMux()
	m3u8Content := `#EXTM3U
#EXTINF:1,
seg1.ts
#EXTINF:1,
seg2.ts
`
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	})
	seg1 := []byte("FIRST")
	seg2 := []byte("SECOND")
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write(seg1)
	})
	mux.HandleFunc("/seg2.ts", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write(seg2)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	outDir := tempDir(t)
	stateDir := tempDir(t)
	cfg := DefaultConfig()
	cfg.OutputDir = outDir
	cfg.ResumeStateDir = stateDir
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.SaveBatchSize = 1
	downloader := NewDownloader(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		res := downloader.Download(ctx, server.URL+"/playlist.m3u8", "resume", "video.ts", nil)
		firstErr = res.Error
	}()
	// 等待第一个片段完成、第二个片段进行中，然后取消
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if firstErr == nil {
		t.Fatal("expected error due to cancel, got nil")
	}
	// 第二次下载应续传
	res2 := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "resume", "video.ts", nil)
	if res2.Error != nil {
		t.Fatalf("resume download failed: %s", res2.Error.Error())
	}
	absPath := filepath.Join(outDir, "resume", "video.ts")
	data, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatal(err)
	}
	expected := append(seg1, seg2...)
	if string(data) != string(expected) {
		t.Errorf("resume content mismatch: got %q, want %q", data, expected)
	}
}

// 测试并发下载相同任务（应串行执行）
func TestConcurrentSameTask(t *testing.T) {
	mux := http.NewServeMux()
	m3u8Content := `#EXTM3U
#EXTINF:1,
seg.ts
`
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(m3u8Content))
	})
	seg := []byte("DATA")
	mux.HandleFunc("/seg.ts", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Write(seg)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	outDir := tempDir(t)
	stateDir := tempDir(t)
	cfg := DefaultConfig()
	cfg.OutputDir = outDir
	cfg.ResumeStateDir = stateDir
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1
	downloader := NewDownloader(cfg)

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		res := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "concurrent", "file.ts", nil)
		err1 = res.Error
	}()
	go func() {
		defer wg.Done()
		res := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "concurrent", "file.ts", nil)
		err2 = res.Error
	}()
	wg.Wait()
	if err1 != nil || err2 != nil {
		t.Errorf("errors: %s, %s", err1.Error(), err2.Error())
	}
}

// ivForSeq 隐式 IV：媒体序号按 128 位大端整数编码（HLS 规范）
func ivForSeq(seq int64) []byte {
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], uint64(seq))
	return iv
}

// encryptAES128CBC 测试辅助：PKCS#7 填充后 AES-128-CBC 加密
func encryptAES128CBC(t *testing.T, plaintext, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

// 隐式 IV 的密钥标签下，每个片段必须用自己的媒体序号推导 IV。
// 回归点：IV 曾跟着密钥一起缓存，导致除首个片段外全部用首个片段的 IV 解密 → 解出垃圾。
func TestDownloadImplicitIVPerSegment(t *testing.T) {
	key := []byte("0123456789abcdef")
	// 媒体序号从 7 起：IV 既不等于 0 也不等于片段下标，两条错误路径都能覆盖
	const mediaSequence = 7
	plainSegments := [][]byte{
		[]byte("SEGMENT-ONE-PLAIN"),
		[]byte("SEGMENT-TWO-PLAIN"),
		[]byte("SEGMENT-THREE-PLAIN"),
	}
	cipherSegments := make([][]byte, len(plainSegments))
	for i, p := range plainSegments {
		cipherSegments[i] = encryptAES128CBC(t, p, key, ivForSeq(mediaSequence+int64(i)))
	}

	mux := http.NewServeMux()
	playlist := fmt.Sprintf(`#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:%d
#EXT-X-KEY:METHOD=AES-128,URI="key.bin"
#EXTINF:1.0,
segment1.ts
#EXTINF:1.0,
segment2.ts
#EXTINF:1.0,
segment3.ts
`, mediaSequence)
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(playlist))
	})
	var keyHits int
	mux.HandleFunc("/key.bin", func(w http.ResponseWriter, r *http.Request) {
		keyHits++
		w.Write(key)
	})
	for i := range cipherSegments {
		data := cipherSegments[i]
		mux.HandleFunc(fmt.Sprintf("/segment%d.ts", i+1), func(w http.ResponseWriter, r *http.Request) {
			w.Write(data)
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.AutoMerge = false // 避免调用 ffmpeg
	cfg.MaxConcurrent = 1
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "enc", "out.ts", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %s", result.Error.Error())
	}
	data, err := os.ReadFile(filepath.Join(cfg.OutputDir, "enc", "out.ts"))
	if err != nil {
		t.Fatal(err)
	}
	expected := bytes.Join(plainSegments, nil)
	if !bytes.Equal(data, expected) {
		t.Errorf("decrypted content mismatch:\n got %q\nwant %q", data, expected)
	}
	if keyHits != 1 {
		t.Errorf("key fetched %d times, want 1 (密钥应被缓存，只有 IV 需要逐片段现算)", keyHits)
	}
}

// prepareKey：隐式 IV 逐片段不同，显式 IV 则所有片段共用同一个
func TestPrepareKeyIV(t *testing.T) {
	key := []byte("0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(key)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	d := NewDownloader(cfg)
	ctx := context.Background()

	// 隐式 IV：IV 由媒体序号推导，不能因为命中密钥缓存就返回首个片段的 IV
	implicit := &KeyInfo{URL: server.URL + "/key.bin"}
	_, iv0, err := d.prepareKey(ctx, implicit, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, iv5, err := d.prepareKey(ctx, implicit, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(iv0, ivForSeq(0)) {
		t.Errorf("iv(seq=0) = %x, want %x", iv0, ivForSeq(0))
	}
	if !bytes.Equal(iv5, ivForSeq(5)) {
		t.Errorf("iv(seq=5) = %x, want %x (命中缓存后仍须按本片段序号推导)", iv5, ivForSeq(5))
	}

	// 显式 IV：与媒体序号无关
	explicit := &KeyInfo{URL: server.URL + "/key.bin", IV: "0x000102030405060708090a0b0c0d0e0f"}
	_, e1, err := d.prepareKey(ctx, explicit, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, e2, err := d.prepareKey(ctx, explicit, 9, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e1, e2) {
		t.Errorf("explicit IV should not depend on media seq: %x vs %x", e1, e2)
	}
}

// EXT-X-MEDIA-SEQUENCE 解析进 MediaSeq；解析失败必须报错而不是当作 0
func TestParseMediaSequence(t *testing.T) {
	d := &Downloader{}
	playlist := `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:100
#EXTINF:1.0,
a.ts
#EXTINF:1.0,
b.ts
`
	_, segs, _, err := d.parsePlaylistEnhancedWithIndex(playlist, "https://example.com/path/")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segs))
	}
	if segs[0].Index != 0 || segs[0].MediaSeq != 100 {
		t.Errorf("seg0: Index=%d MediaSeq=%d, want 0/100", segs[0].Index, segs[0].MediaSeq)
	}
	if segs[1].Index != 1 || segs[1].MediaSeq != 101 {
		t.Errorf("seg1: Index=%d MediaSeq=%d, want 1/101", segs[1].Index, segs[1].MediaSeq)
	}

	// 缺省时媒体序号从 0 起
	_, segs, _, err = d.parsePlaylistEnhancedWithIndex("#EXTM3U\n#EXTINF:1.0,\na.ts\n", "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if segs[0].MediaSeq != 0 {
		t.Errorf("default MediaSeq = %d, want 0", segs[0].MediaSeq)
	}

	bad := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:abc\n#EXTINF:1.0,\na.ts\n"
	if _, _, _, err := d.parsePlaylistEnhancedWithIndex(bad, "https://example.com/"); err == nil {
		t.Error("expected error for malformed EXT-X-MEDIA-SEQUENCE, got nil")
	}
}

// 显式 IV 的长度是输入校验边界：cipher.NewCBCDecrypter 对非 16 字节的 IV 直接 panic，
// 而 IV 来自远端播放列表 —— 非法输入必须在这里变成 error，不能带进 crypto/cipher。
func TestParseIVValidatesLength(t *testing.T) {
	d := &Downloader{}
	cases := []struct {
		name    string
		iv      string
		wantErr bool
	}{
		{"标准 16 字节", "0x000102030405060708090a0b0c0d0e0f", false},
		{"不带 0x 前缀", "000102030405060708090a0b0c0d0e0f", false},
		{"奇数长度补零后正好 16 字节", "0x" + strings.Repeat("ab", 15) + "a", false},
		{"太短：IV=0x0102", "0x0102", true},
		{"奇数长度、补零后仍不足", "0x010", true},
		{"太长：32 字节", "0x" + strings.Repeat("ab", 32), true},
		{"空 IV", "0x", true},
		{"非法 hex", "0xzz", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iv, err := d.parseIV(tc.iv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseIV(%q) = %x, want error", tc.iv, iv)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseIV(%q) = %v", tc.iv, err)
			}
			if len(iv) != aes.BlockSize {
				t.Fatalf("iv 长度 = %d, want %d", len(iv), aes.BlockSize)
			}
		})
	}
}

// 端到端：播放列表里写一个非 16 字节的 IV，下载必须返回 error 而不是把进程打崩。
func TestDownloadBadIVDoesNotPanic(t *testing.T) {
	key := []byte("0123456789abcdef")
	payload := []byte("0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/key.bin"):
			w.Write(key)
		case strings.HasSuffix(r.URL.Path, ".ts"):
			w.Write(payload)
		default:
			w.Write([]byte("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n" +
				`#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x0102` + "\n" +
				"#EXTINF:1.0,\nseg0.ts\n#EXT-X-ENDLIST\n"))
		}
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxRetries = 1 // 别在重试上耗时间，只验证「报错而不是崩」
	d := NewDownloader(cfg)

	res := d.Download(context.Background(), server.URL+"/playlist.m3u8", "", "video.ts", nil)
	if res == nil || res.Error == nil {
		t.Fatalf("非 16 字节的 IV 应当失败，实得 %+v", res)
	}
	if !strings.Contains(res.Error.Error(), "invalid IV length") {
		t.Fatalf("错误信息里应说清是 IV 长度问题，实得：%v", res.Error)
	}
}

// 限速：要发的字节数超过"桶大小"时不能直接失败。
//
// 回归点：rate.Limiter.WaitN 对 n > burst 是**立即报错**（"exceeds limiter's burst"），
// 而这里的桶大小取的是一秒的额度 —— HLS 片段动辄几百 KB，于是限速一开、稍大的片段必失败，
// 这个功能实际等于不可用。修法是按 burst 分批等：速率不变，任意大小都能过。
func TestRateLimiterHandlesPayloadLargerThanBurst(t *testing.T) {
	rl := NewRateLimiter(1) // 1 KB/s → burst = 1024 字节

	// 小于桶大小：桶里本来就有额度，应当立刻通过
	if err := rl.Wait(context.Background(), 512); err != nil {
		t.Fatalf("不大于 burst 应当立刻通过：%v", err)
	}

	// 远大于桶大小：应当"等"到 ctx 超时，而不是立刻甩一句 burst 错
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := rl.Wait(ctx, 8192) // 8 倍 burst
	if err == nil {
		t.Fatal("额度不够时应当等到 ctx 超时，不能无声通过")
	}
	if strings.Contains(err.Error(), "burst") {
		t.Fatalf("n 超过桶大小不该直接失败：%v", err)
	}
}

// 分片写入必须经过中转：写失败时 destPath 上不能出现半成品。
//
// 回归点：原来直接写 destPath，进程被杀或写失败会留下**非空的半截分片**，
// 而"已存在且 size>0 就跳过"会把它当成下好的 → 静默合并出损坏的媒体。
func TestSegmentWriteIsAtomicOnFailure(t *testing.T) {
	payload := []byte("0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxRetries = 0
	d := NewDownloader(cfg)

	dir := tempDir(t)
	dest := filepath.Join(dir, "segment_00000.ts")
	// 把中转路径占成一个目录 → 写入必定失败（Windows 与 Unix 都不允许往目录里写）
	if err := os.Mkdir(dest+".tmp", 0755); err != nil {
		t.Fatal(err)
	}

	seg := &SegmentInfo{Index: 0, URL: server.URL + "/seg.ts"}
	if err := d.downloadSegmentToFile(context.Background(), seg, nil, nil, dest, nil); err == nil {
		t.Fatal("写入失败时应当返回错误")
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("写入失败后 destPath 上不该出现半成品 —— 它下一轮会被当成『已下载』跳过")
	}
}

// 成功路径：内容完整，且不留下中转文件。
func TestSegmentWriteSucceedsWithoutResidue(t *testing.T) {
	payload := []byte("0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	d := NewDownloader(cfg)

	dir := tempDir(t)
	dest := filepath.Join(dir, "segment_00000.ts")
	seg := &SegmentInfo{Index: 0, URL: server.URL + "/seg.ts"}
	if err := d.downloadSegmentToFile(context.Background(), seg, nil, nil, dest, nil); err != nil {
		t.Fatalf("下载应当成功：%v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("destPath 内容 = %q, want %q", got, payload)
	}
	if _, err := os.Stat(dest + ".tmp"); err == nil {
		t.Fatal("成功之后不该留下中转文件")
	}
}

// MaxConcurrent 为 0 时不能让下载卡死。
//
// 回归点：并发上限原来是直接 `make(chan struct{}, cfg.MaxConcurrent)` —— 0 就是**无缓冲**通道，
// 第一个 goroutine 永久阻塞在 `sem <- struct{}{}`，wg.Wait() 再也不返回：
// 不报错、不退出、一条日志都没有，业务看到的就是"下载卡死"。
// NewDownloader 只在 cfg == nil 时补默认值（DefaultConfig 里是 5），
// 所以"自己拼了一份 Config 但漏了/填错这个字段"必然踩到。
//
// 这里连负数一起验：make(chan, -1) 会直接 panic。
func TestDownloadDoesNotHangOnNonPositiveConcurrency(t *testing.T) {
	for _, concurrency := range []int{0, -1} {
		t.Run(fmt.Sprintf("MaxConcurrent=%d", concurrency), func(t *testing.T) {
			segments := [][]byte{[]byte("SEG-0"), []byte("SEG-1"), []byte("SEG-2")}

			mux := http.NewServeMux()
			mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
				var b strings.Builder
				b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:2\n")
				for i := range segments {
					fmt.Fprintf(&b, "#EXTINF:1.0,\nsegment%d.ts\n", i)
				}
				_, _ = w.Write([]byte(b.String()))
			})
			for i, body := range segments {
				content := body
				mux.HandleFunc(fmt.Sprintf("/segment%d.ts", i), func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(content)
				})
			}
			server := httptest.NewServer(mux)
			defer server.Close()

			cfg := DefaultConfig()
			cfg.OutputDir = tempDir(t)
			cfg.ResumeStateDir = tempDir(t)
			cfg.AutoMerge = false // 避免调用 ffmpeg
			cfg.MaxConcurrent = concurrency

			d := NewDownloader(cfg)

			// 下载放 goroutine：卡住时靠超时把测试**干脆地**判失败，而不是挂到 go test 的全局超时
			done := make(chan *DownloadResult, 1)
			go func() {
				done <- d.Download(context.Background(), server.URL+"/playlist.m3u8", "out", "zero.ts", nil)
			}()

			select {
			case result := <-done:
				if result.Error != nil {
					t.Fatalf("下载应当成功：%v", result.Error)
				}
				if result.Segments != len(segments) {
					t.Fatalf("Segments = %d, want %d", result.Segments, len(segments))
				}
				got, err := os.ReadFile(filepath.Join(cfg.OutputDir, "out", "zero.ts"))
				if err != nil {
					t.Fatal(err)
				}
				var want []byte
				for _, s := range segments {
					want = append(want, s...)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("拼接结果 = %q, want %q（顺序不能乱）", got, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("MaxConcurrent=%d 时下载卡死了：信号量无缓冲，wg.Wait() 永不返回", concurrency)
			}
		})
	}
}

// 同上一条（filedown）：doRequest 的重试退避要能被 ctx 打断。
func TestRetryBackoffIsInterruptible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 每次尝试都失败，逼出退避
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MaxRetries = 3
	cfg.RetryInterval = 3 // 第 1 次退避 = 3s
	d := NewDownloader(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.doRequest(ctx, srv.URL, "", nil)
		done <- err
	}()

	time.Sleep(300 * time.Millisecond) // 等第一次尝试失败、进入退避
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消 ctx 后应当以错误结束")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("取消 ctx 后仍在睡退避 —— 退避没接 ctx")
	}
}

// 与 filedown 同一条约定：成功的流程不写错误通道。
// 这条走多码率主播放列表，一次覆盖两处 —— "选择最高码率流"（一次下载一行 ERROR）
// 与"片段 x/y 完成"（每片段一行）。
func TestSuccessfulDownloadKeepsErrorChannelQuiet(t *testing.T) {
	mux := http.NewServeMux()
	master := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1280000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5120000
high.m3u8
`
	media := `#EXTM3U
#EXT-X-TARGETDURATION:2
#EXTINF:1.0,
segment1.ts
#EXTINF:1.0,
segment2.ts
`
	mux.HandleFunc("/master.m3u8", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(master)) })
	for _, name := range []string{"/low.m3u8", "/high.m3u8"} {
		mux.HandleFunc(name, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(media)) })
	}
	for _, name := range []string{"/segment1.ts", "/segment2.ts"} {
		mux.HandleFunc(name, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("SEGMENT DATA")) })
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.AutoMerge = false // 避免调用 ffmpeg
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.SaveBatchSize = 1
	cfg.QueueSize = 100
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL+"/master.m3u8", "subdir", "out.ts", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %s", result.Error.Error())
	}
	if result.Segments != 2 {
		t.Fatalf("Segments = %d, want 2", result.Segments)
	}

	select {
	case err := <-downloader.GetErrors():
		t.Fatalf("成功的下载不该往错误通道写东西，实得：%v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// OutputDir() 是 DownloadResult.OutputFile 的基准：两者一拼就是那个文件（与 filedown 同一个约定，
// `result.OutputFile` 里不带输出根目录）。调用方靠它把相对路径变成能落库的本地路径。
func TestOutputDirGetterIsTheBaseOfOutputFile(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\nsegment1.ts\n"))
	})
	mux.HandleFunc("/segment1.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("SEGMENT1 DATA"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.AutoMerge = false // 避免调用 ffmpeg
	cfg.MaxConcurrent = 1
	downloader := NewDownloader(cfg)

	result := downloader.Download(context.Background(), server.URL+"/playlist.m3u8", "movies", "ep1.ts", nil)
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}

	if got := downloader.OutputDir(); got != cfg.OutputDir {
		t.Fatalf("OutputDir() = %q, want %q（配置里写了什么就返回什么）", got, cfg.OutputDir)
	}
	abs := filepath.Join(downloader.OutputDir(), result.OutputFile)
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("拼出来的路径 %q 读不到刚下的文件：%v", abs, err)
	}
}

// 没传配置时走 DefaultConfig，getter 不能返回空串。
func TestOutputDirGetterDefaultsWhenConfigNil(t *testing.T) {
	if got, want := NewDownloader(nil).OutputDir(), DefaultConfig().OutputDir; got != want {
		t.Fatalf("OutputDir() = %q, want %q", got, want)
	}
}

// 下载器不读 ctx：抓取上下文要显式转成下载选项（与 filedown.OptionsFromRequest 同一个约定）。
func TestOptionsFromRequestCarriesHeadersAndProxy(t *testing.T) {
	ctx := core.WithHeaders(context.Background(), map[string]string{
		"User-Agent": "ua-site",
		"Cookie":     "sid=1",
		"X-Drop":     "", // 空值 = 删掉这个头
	})
	ctx = core.WithProxyURL(ctx, "http://127.0.0.1:8080")

	opts := OptionsFromRequest(ctx, "https://site.example/detail/1")
	if opts.Referer != "https://site.example/detail/1" {
		t.Fatalf("Referer = %q", opts.Referer)
	}
	if opts.Proxy != "http://127.0.0.1:8080" {
		t.Fatalf("Proxy = %q，ctx 上指定了出口就该带上", opts.Proxy)
	}
	if opts.Headers["User-Agent"] != "ua-site" || opts.Headers["Cookie"] != "sid=1" {
		t.Fatalf("ctx 上的头没带过来：%v", opts.Headers)
	}
	// 空值 = "删掉这个头"（与抓取路径同一套语义），不能变成发出去一个空头
	if _, ok := opts.Headers["X-Drop"]; ok {
		t.Fatalf("空值的头应当被删掉而不是留着：%v", opts.Headers)
	}

	// 头是副本：改选项不该影响 ctx 里那份
	opts.Headers["User-Agent"] = "tampered"
	if core.HeadersFrom(ctx)["User-Agent"] != "ua-site" {
		t.Fatal("OptionsFromRequest 返回的头应当是副本")
	}
}

// ctx 上什么都没有：只带 Referer；Headers 是**可写的空 map**而不是 nil ——
// 调用方紧接着就要补自己的键（`opts.Headers["Accept"] = ...`），给 nil map 就是等着 panic。
func TestOptionsFromRequestWithoutContext(t *testing.T) {
	opts := OptionsFromRequest(context.Background(), "https://site.example/p")
	if opts.Referer != "https://site.example/p" || opts.Proxy != "" {
		t.Fatalf("空 ctx 应当只带 Referer：%+v", opts)
	}
	if opts.Headers == nil || len(opts.Headers) != 0 {
		t.Fatalf("Headers 应当是非 nil 的空 map：%#v", opts.Headers)
	}
	opts.Headers["Accept"] = "image/webp" // 不该 panic
	if len(core.HeadersFrom(context.Background())) != 0 {
		t.Fatal("补键不该影响到 ctx")
	}
}

// 端到端：头落到**分片请求**上（不是播放列表那一下），且来源页以参数为准 ——
// 分片请求是先写 Referer 字段、后写 Headers map，ctx 里那个 Referer 会盖掉参数，
// 所以 OptionsFromRequest 把它删掉了。
func TestOptionsFromRequestReachesSegmentRequest(t *testing.T) {
	var got http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\nsegment1.ts\n"))
	})
	mux.HandleFunc("/segment1.ts", func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte("SEGMENT1 DATA"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.AutoMerge = false
	cfg.MaxConcurrent = 1

	ctx := core.WithHeaders(context.Background(), map[string]string{
		"User-Agent": "ua-site",
		"Referer":    "https://ctx.example/",
	})
	opts := OptionsFromRequest(ctx, server.URL+"/detail/1")
	res := NewDownloader(cfg).Download(ctx, server.URL+"/playlist.m3u8", "movies", "ep1.ts", opts)
	if res.Error != nil {
		t.Fatalf("download failed: %v", res.Error)
	}
	if got.Get("User-Agent") != "ua-site" {
		t.Fatalf("分片请求上没带上 ctx 的 UA：%v", got)
	}
	if got.Get("Referer") != server.URL+"/detail/1" {
		t.Fatalf("Referer = %q，参数里的来源页应当优先于 ctx 里那个", got.Get("Referer"))
	}
	// 顺带钉住：解密依赖的 Accept-Encoding 不受 ctx 影响
	if got.Get("Accept-Encoding") != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got.Get("Accept-Encoding"))
	}
}
