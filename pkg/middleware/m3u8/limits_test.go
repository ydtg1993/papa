package m3u8

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 带 Range 的请求：期望长度就写在 Range 里，据此设上界。
// 无视 Range 塞回超大 body 的服务器，以前会被 io.ReadAll 一路吃内存，现在直接失败。
func TestOversizedRangeResponseIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent) // 声明 206，却无视 Range 塞回 4KB
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MaxRetries = 0 // 不重试，直接把错误抛出来
	d := NewDownloader(cfg)

	data, err := d.doRequest(context.Background(), srv.URL, "bytes=0-9", nil)
	if err == nil {
		t.Fatalf("超长响应应当报错，而不是被静默收下（收到 %d 字节）", len(data))
	}
	if !strings.Contains(err.Error(), "超过") {
		t.Fatalf("错误信息应说明超过上界，实得 %v", err)
	}
}

// 反向：长度正好等于请求区间时照常收下（别把上界写成"一律拒绝"）。
func TestRangeResponseWithinLimitIsAccepted(t *testing.T) {
	body := []byte("0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.MaxRetries = 0
	d := NewDownloader(cfg)

	got, err := d.doRequest(context.Background(), srv.URL, "bytes=0-9", nil)
	if err != nil {
		t.Fatalf("正好等于区间长度应当放行：%v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("内容不一致：%q", got)
	}
}

// rangeLength 是"上界从哪来"的那一半，单独钉住格式识别 ——
// 认不出来就退回无界读（宁可保持原样，也不凭空定一个"分片最大多少"）。
func TestRangeLength(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes=0-9", 10, true},
		{"bytes=100-199", 100, true},
		{"bytes=5-5", 1, true},
		{"bytes=10-9", 0, false}, // 区间反了
		{"bytes=-9", 0, false},   // 后缀式 Range，这里不认
		{"items=0-9", 0, false},  // 单位不对
		{"bytes=a-b", 0, false},  // 不是数字
		{"", 0, false},
	} {
		got, ok := rangeLength(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("rangeLength(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
