package m3u8

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 叠加层：nil = 不动；写了就连零值也认（0 并发、0 限速、false 这些都得能表达）。
func TestOverlayApply(t *testing.T) {
	base := DefaultConfig()

	// 空覆盖层：一个字段都不该动
	if got := (Overlay{}).Apply(*base); !reflect.DeepEqual(got, *base) {
		t.Fatalf("空覆盖层不该改动任何字段\n got=%+v\nwant=%+v", got, *base)
	}
	if !(Overlay{}).IsZero() {
		t.Fatal("空覆盖层 IsZero 应当为 true")
	}

	mode := OutputSegments
	dir := "out"
	state := "state"
	ext := ".mkv"
	keep := false
	concurrent := 0 // 零值也要能覆盖（否则"想关掉并发"表达不出来）
	timeout := 45
	retries := 0
	interval := 7
	resume := false
	rate := 0
	batch := 3
	ffmpeg := "/opt/ffmpeg"
	queue := 8
	o := Overlay{
		OutputDir: &dir, ResumeStateDir: &state, Output: &mode, MergeExt: &ext, KeepSegments: &keep,
		MaxConcurrent: &concurrent, SegmentTimeout: &timeout, MaxRetries: &retries, RetryInterval: &interval,
		EnableResume: &resume, RateKB: &rate, SaveBatchSize: &batch, FfmpegPath: &ffmpeg, QueueSize: &queue,
	}
	got := o.Apply(*base)
	if got.OutputDir != dir || got.ResumeStateDir != state || got.OutputMode != mode ||
		got.MergeOutputExt != ext || got.KeepSegmentsAfterMerge != keep ||
		got.MaxConcurrent != 0 || got.SegmentTimeout != 45*time.Second || got.MaxRetries != 0 ||
		got.RetryInterval != 7 || got.EnableResume != false || got.RateKB != 0 ||
		got.SaveBatchSize != 3 || got.FfmpegPath != ffmpeg || got.QueueSize != 8 {
		t.Fatalf("覆盖结果不对：%+v", got)
	}
	// 基座不能被改（Apply 是纯函数）
	if base.OutputDir == dir || base.OutputMode == mode {
		t.Fatal("Apply 不该改动传入的 Config")
	}
}

// 层级解析：阶段覆盖赢过站点覆盖（`.Then` 的语义就是"右边赢"）。
func TestOverlayThenStageWins(t *testing.T) {
	siteDir, stageDir := "site", "stage"
	concurrent := 5
	site := Overlay{OutputDir: &siteDir, MaxConcurrent: &concurrent}
	stage := Overlay{OutputDir: &stageDir}

	merged := site.Then(stage)
	if merged.OutputDir == nil || *merged.OutputDir != stageDir {
		t.Fatalf("阶段那层应当赢：%+v", merged.OutputDir)
	}
	if merged.MaxConcurrent == nil || *merged.MaxConcurrent != concurrent {
		t.Fatalf("阶段没写的项应当保留站点那层的值：%+v", merged.MaxConcurrent)
	}
	// 两层都不写 = 还是空的
	if !(Overlay{}).Then(Overlay{}).IsZero() {
		t.Fatal("两层都空，合并结果应当还是空")
	}
}

// Fields 只列被改过的项 —— papa check 与启动日志靠它说清"这一层到底动了什么"。
func TestOverlayFields(t *testing.T) {
	if got := (Overlay{}).Fields(); len(got) != 0 {
		t.Fatalf("空覆盖层不该列出任何项：%v", got)
	}
	mode := OutputPlaylist
	keep := true
	o := Overlay{Output: &mode, KeepSegments: &keep}
	got := strings.Join(o.Fields(), ",")
	for _, want := range []string{"output=playlist", "keep_segments=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("Fields() 缺少 %q：%s", want, got)
		}
	}
	if strings.Contains(got, "output_dir") || strings.Contains(got, "merge_ext") {
		t.Errorf("没改的项不该出现：%s", got)
	}
}

// With：**配置独立、状态共享** —— 这是"随用随生成"与"同一目标文件串行"能同时成立的关键。
func TestWithSharesStateAndIsolatesConfig(t *testing.T) {
	base := NewDownloader(DefaultConfig())
	concurrent := 2
	fork := base.With(Overlay{MaxConcurrent: &concurrent})

	if fork.st != base.st {
		t.Fatal("With 出来的实例应当共用内部状态（labors/密钥缓存/代理连接池/消息队列）")
	}
	if fork.Config().MaxConcurrent != 2 {
		t.Fatalf("fork 的配置应当被覆盖，实得 %d", fork.Config().MaxConcurrent)
	}
	if got := base.Config().MaxConcurrent; got != DefaultConfig().MaxConcurrent {
		t.Fatalf("原实例的配置不该被动过，实得 %d", got)
	}
	// 状态共享的可见效果：一边往 labors 里放一条，另一边立刻看得到
	base.st.laborMu.Lock()
	base.st.labors["k"] = &labor{source: "s", outDir: "d", filename: "f"}
	base.st.laborMu.Unlock()
	fork.st.laborMu.RLock()
	_, ok := fork.st.labors["k"]
	fork.st.laborMu.RUnlock()
	if !ok {
		t.Fatal("fork 看不到原实例的 labor —— 那样两条并发下载就会各写各的同一个文件")
	}
	// 孤岛：NewDownloader 造出来的是另一份状态
	if NewDownloader(DefaultConfig()).st == base.st {
		t.Fatal("NewDownloader 应当是互不共享的一份（要共享用 With）")
	}
}

// 只存播放列表：一个分片都不下（顺手钉住"不碰断点状态目录"）。
func TestDownloadPlaylistModeWritesPlaylistOnly(t *testing.T) {
	var segmentHits atomic.Int64
	mux := http.NewServeMux()
	playlistText := "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1.0,\nseg0.ts\n#EXTINF:1.0,\nseg1.ts\n#EXT-X-ENDLIST\n"
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, playlistText)
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, r *http.Request) {
		segmentHits.Add(1)
		fmt.Fprint(w, "segment")
	})
	mux.HandleFunc("/seg1.ts", func(w http.ResponseWriter, r *http.Request) {
		segmentHits.Add(1)
		fmt.Fprint(w, "segment")
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	dl := NewDownloader(cfg)

	mode := OutputPlaylist
	res := dl.Download(context.Background(), server.URL+"/playlist.m3u8", "sub", "index.m3u8", &DownloadOptions{Output: &mode})
	if res.Error != nil {
		t.Fatalf("Download: %v", res.Error)
	}
	if hits := segmentHits.Load(); hits != 0 {
		t.Fatalf("只存播放列表的形态不该请求任何分片，实得 %d 次", hits)
	}
	if res.Segments != 0 || res.Size != int64(len(playlistText)) {
		t.Fatalf("结果不对：segments=%d size=%d", res.Segments, res.Size)
	}
	want := filepath.Join(cfg.OutputDir, "sub", "index.m3u8")
	if res.OutputFile != filepath.Join("sub", "index.m3u8") {
		t.Fatalf("OutputFile 应当相对 OutputDir，实得 %q", res.OutputFile)
	}
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("播放列表没落盘：%v", err)
	}
	if string(data) != playlistText {
		t.Fatalf("落盘的不是播放列表原文：%q", string(data))
	}
}

// 只留切片组：分片被挪到 `<OutputDir>/<outDir>/<file>/` 下，不产出单文件。
func TestDownloadSegmentsModeKeepsSegmentDir(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1.0,\nseg0.ts\n#EXTINF:1.0,\nseg1.ts\n#EXTINF:1.0,\nseg2.ts\n#EXT-X-ENDLIST\n")
	})
	for _, name := range []string{"seg0.ts", "seg1.ts", "seg2.ts"} {
		name := name
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", 32))
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.SaveBatchSize = 1
	dl := NewDownloader(cfg)

	mode := OutputSegments
	res := dl.Download(context.Background(), server.URL+"/playlist.m3u8", "sub", "movie", &DownloadOptions{Output: &mode})
	if res.Error != nil {
		t.Fatalf("Download: %v", res.Error)
	}
	if res.OutputFile != filepath.Join("sub", "movie") {
		t.Fatalf("切片组形态的 OutputFile 应当是那个目录（相对 OutputDir），实得 %q", res.OutputFile)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.OutputDir, "sub", "movie"))
	if err != nil {
		t.Fatalf("切片目录不在：%v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("切片数 = %d, want 3（文件名：%v）", len(entries), entries)
	}
	// 不该产出单文件（合并/拼接都不做）
	if _, err := os.Stat(filepath.Join(cfg.OutputDir, "sub", "movie.ts")); err == nil {
		t.Fatal("只留切片的形态不该产出单文件")
	}
	if _, err := os.Stat(filepath.Join(cfg.OutputDir, "sub", "movie.mp4")); err == nil {
		t.Fatal("只留切片的形态不该产出单文件")
	}
}

// 走代理的客户端必须和默认那份一样带 SegmentTimeout。
//
// 回归点：代理那份以前只设了 Transport（连接池调优），**没有 Timeout** —— 而 doRequest 用的是
// 任务 ctx（没有逐请求 deadline），代理"接了 TCP 不回包"时 Do 永久阻塞：不报错、不重试，
// worker 抱着并发位挂死。这条按"取到的客户端长什么样"钉住，不依赖真代理。
func TestProxyClientInheritsSegmentTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SegmentTimeout = 7 * time.Second
	dl := NewDownloader(cfg)

	plain := dl.clientFor(&DownloadOptions{})
	if plain.Timeout != 7*time.Second {
		t.Fatalf("默认客户端 Timeout = %v, want 7s", plain.Timeout)
	}
	proxied := dl.clientFor(&DownloadOptions{Proxy: "http://127.0.0.1:9"})
	if proxied == plain {
		t.Fatal("指定了代理就该拿到另一份客户端")
	}
	if proxied.Timeout != 7*time.Second {
		t.Fatalf("代理客户端 Timeout = %v, want 7s（否则代理挂住会把 worker 卡死）", proxied.Timeout)
	}
	// 同一个代理地址要复用同一份（连接池不白扔）
	if again := dl.clientFor(&DownloadOptions{Proxy: "http://127.0.0.1:9"}); again != proxied {
		t.Fatal("同一代理地址应当复用同一个客户端")
	}
}
