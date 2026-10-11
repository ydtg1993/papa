package m3u8

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// `#EXT-X-BYTERANGE` 的两种写法都要换算成**合法的 HTTP Range**，省略 @起点 的按
// "同一资源上一段的末尾"接着算（RFC 8216 §4.3.2.2）。
func TestParseByteRange(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		prevEnd int64
		want    string
		wantErr bool
	}{
		{"显式起点", "1024@0", 0, "0-1023", false},
		{"显式起点不在 0", "1024@2048", 0, "2048-3071", false},
		{"省略起点：接着上一段", "1024", 1024, "1024-2047", false},
		{"省略起点且是资源第一段：从 0 算", "512", 0, "0-511", false},
		{"长度是 1 也合法", "1@7", 0, "7-7", false},
		{"空格容忍", " 100 @ 5 ", 0, "5-104", false},
		{"长度不是数", "abc@0", 0, "", true},
		{"长度是 0", "0@0", 0, "", true},
		{"长度是负的", "-10@0", 0, "", true},
		{"起点不是数", "10@x", 0, "", true},
		{"起点是负的", "10@-1", 0, "", true},
		{"空", "", 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseByteRange(tc.spec, tc.prevEnd)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseByteRange(%q) 该报错，实得 %s", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseByteRange(%q) = %v", tc.spec, err)
			}
			if got.String() != tc.want {
				t.Fatalf("parseByteRange(%q, prevEnd=%d) = %s, want %s", tc.spec, tc.prevEnd, got, tc.want)
			}
		})
	}
}

// 端到端：同一个媒体资源被切成三段（第一段显式 @0、后两段省略 @起点），产物必须是
// 三段按顺序拼起来的**整个资源**。
//
// 这条是"静默产出错文件"那个 bug 的验收：老写法发出的 Range 头是 `bytes=100@0`，
// ServeContent 判定为非法 Range 就忽略它、回 300 字节整份 —— 每个分片都拿到整份，
// 产物 900 字节且内容重复，测试立刻红。
func TestDownloadByteRangeSegments(t *testing.T) {
	// 三段内容互不相同，拼错顺序/落错区间都看得出来
	body := append(append(bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 100)...), bytes.Repeat([]byte("c"), 100)...)

	playlist := "#EXTM3U\n#EXT-X-VERSION:4\n#EXT-X-TARGETDURATION:1\n" +
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:100@0\nfile.ts\n" +
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:100\nfile.ts\n" +
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:100\nfile.ts\n" +
		"#EXT-X-ENDLIST\n"

	var (
		rangeMu sync.Mutex
		ranges  []string
	)
	mux := http.NewServeMux()
	// 记下**实际发出去的 Range 头**：这条断言与"服务端怎么处理非法 Range"无关，
	// 是"我们发的是不是合法 Range"的直接判据。Go 的 ServeContent 对非法 Range 回 416。
	mux.HandleFunc("/file.ts", func(w http.ResponseWriter, r *http.Request) {
		rangeMu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		rangeMu.Unlock()
		http.ServeContent(w, r, "file.ts", time.Time{}, bytes.NewReader(body))
	})
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, playlist)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxConcurrent = 1
	cfg.AutoMerge = false // 纯拼接：产物就是三段字节按序相接，不用 ffmpeg
	// 关掉重试：这条测的是"要哪些字节"，不要被退避重试拖成几十秒（退避是另一条契约）
	cfg.MaxRetries = 0
	dl := NewDownloader(cfg)

	res := dl.Download(context.Background(), server.URL+"/playlist.m3u8", "sub", "movie.ts")
	if res.Error != nil {
		t.Fatalf("Download: %v", res.Error)
	}
	if res.Segments != 3 {
		t.Fatalf("应当解析出 3 个分片，实得 %d", res.Segments)
	}
	got, err := os.ReadFile(filepath.Join(cfg.OutputDir, "sub", "movie.ts"))
	if err != nil {
		t.Fatalf("产物不在：%v", err)
	}
	// 发出去的必须是合法 Range（老写法是 `bytes=100@0` 这种，两种服务端都受不了）。
	// 三段是并发的，请求**到达顺序**不定，比集合就行。
	rangeMu.Lock()
	gotRanges := append([]string(nil), ranges...)
	rangeMu.Unlock()
	sort.Strings(gotRanges)
	wantRanges := []string{"bytes=0-99", "bytes=100-199", "bytes=200-299"}
	if len(gotRanges) != len(wantRanges) {
		t.Fatalf("Range 头 %v, want %v", gotRanges, wantRanges)
	}
	for i, w := range wantRanges {
		if gotRanges[i] != w {
			t.Fatalf("第 %d 次请求的 Range = %q, want %q（全部：%v）", i, gotRanges[i], w, gotRanges)
		}
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("产物不是三段按序拼起来的整个资源：%d 字节，want %d（前 8 字节 %q）",
			len(got), len(body), string(got[:min(8, len(got))]))
	}
}

// 省略 @起点 的分片必须接着**同一资源**上一段算；换了资源就从头算（RFC 8216 §4.3.2.2）。
func TestParseByteRangeTracksPerResource(t *testing.T) {
	d := &Downloader{}
	playlist := "#EXTM3U\n#EXT-X-VERSION:4\n" +
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:100@0\nfile.ts\n" +
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:100\nfile.ts\n" +
		// 换了一个资源：省略起点时从 0 起算，而不是接着 file.ts 的 200
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:100\nother.ts\n" +
		"#EXTINF:1.0,\n#EXT-X-BYTERANGE:50\nfile.ts\n" + // 回到 file.ts：接着 200 继续
		"#EXT-X-ENDLIST\n"
	_, segs, _, err := d.parsePlaylistEnhancedWithIndex(playlist, "https://example.com/p/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0-99", "100-199", "0-99", "200-249"}
	if len(segs) != len(want) {
		t.Fatalf("分片数 = %d, want %d", len(segs), len(want))
	}
	for i, w := range want {
		if segs[i].Range != w {
			t.Errorf("第 %d 段的 Range = %s, want %s（URL %s）", i, segs[i].Range, w, segs[i].URL)
		}
	}
}

// 解析不出来的 BYTERANGE 要硬报错：放过去就是"静默退化成整段下载"，产物错了也没人知道。
func TestParsePlaylistRejectsBadByteRange(t *testing.T) {
	d := &Downloader{}
	playlist := "#EXTM3U\n#EXTINF:1.0,\n#EXT-X-BYTERANGE:oops\nfile.ts\n"
	if _, _, _, err := d.parsePlaylistEnhancedWithIndex(playlist, "https://example.com/p/"); err == nil {
		t.Fatal("非法 BYTERANGE 应当报错，而不是当成整段下载")
	}
}
