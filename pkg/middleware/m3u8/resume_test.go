package m3u8

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 断点状态与磁盘对不上（状态说"全下好了"、分片却一个都不在）时：
// 必须**清掉状态文件再报错**，让下一次跑从头重下。
//
// 这正是"先删分片、后清状态"那个顺序留下的残局：进程死在这两步之间，状态文件还在、
// 分片已经没了。旧写法每次跑都跳过所有下载直接进合并 → 撞"文件不存在" → 失败在清理之前
// 返回 → 状态文件照旧 → 这个任务**永久失败**，只能人工去删 <ResumeStateDir>/<outDir>/<hash>.json。
func TestDownloadSelfHealsStaleResumeState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1.0,\nseg0.ts\n#EXTINF:1.0,\nseg1.ts\n#EXT-X-ENDLIST\n")
	})
	for _, name := range []string{"seg0.ts", "seg1.ts"} {
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
	cfg.AutoMerge = false // 纯拼接收尾，不依赖 ffmpeg
	dl := NewDownloader(cfg)

	const outDir, outFile = "sub", "movie"
	url := server.URL + "/playlist.m3u8"
	la := &labor{source: url, outDir: outDir, filename: outFile}
	// 手工造一份"两个分片都下好了"的状态；分片文件一个都不建 —— 就是残局那一刻的样子
	stale := &ResumeState{
		M3U8URL:       url,
		OutputFile:    outFile,
		TotalSegments: 2,
		Completed:     []int{0, 1},
		SegmentFiles: []string{
			filepath.Join(cfg.ResumeStateDir, "segments", outDir, outFile, "segment_00000.ts"),
			filepath.Join(cfg.ResumeStateDir, "segments", outDir, outFile, "segment_00001.ts"),
		},
	}
	if err := dl.saveResumeState(la, stale); err != nil {
		t.Fatalf("造状态文件：%v", err)
	}
	statePath := dl.getStateFilePath(la)

	mode := OutputSegments
	res := dl.Download(context.Background(), url, outDir, outFile, &DownloadOptions{Output: &mode})
	if res.Error == nil {
		t.Fatal("分片缺失时应当报错，而不是硬着头皮去合并")
	}
	if !strings.Contains(res.Error.Error(), "segment file missing") {
		t.Fatalf("报错该说清是分片缺失，实得：%v", res.Error)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("报错时该顺手清掉状态文件，否则下一次跑还是栽在同一个地方")
	}

	// 自愈的验收：再跑一次必须真去下载（而不是继续跳过下载、继续撞合并）
	res2 := dl.Download(context.Background(), url, outDir, outFile, &DownloadOptions{Output: &mode})
	if res2.Error != nil {
		t.Fatalf("清掉状态后重跑该从头下成功，实得：%v", res2.Error)
	}
	entries, err := os.ReadDir(filepath.Join(cfg.OutputDir, outDir, outFile))
	if err != nil {
		t.Fatalf("切片目录不在：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("重下应当拿到 2 个切片，实得 %d（%v）", len(entries), entries)
	}
}

// 收尾顺序：**先清状态文件，再删分片**。反过来则崩在中间就留下残局（见上一条测试）。
// 顺序本身没法在同一个函数里"观察"，只能钉住结果：正常跑完之后状态文件与分片都不该还在。
func TestDownloadCleansStateFileAfterSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:1.0,\nseg0.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/seg0.ts", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 32))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.MaxConcurrent = 1
	cfg.EnableResume = true
	cfg.AutoMerge = false
	dl := NewDownloader(cfg)

	const outDir, outFile = "sub", "movie"
	url := server.URL + "/playlist.m3u8"
	la := &labor{source: url, outDir: outDir, filename: outFile}

	mode := OutputSegments
	if res := dl.Download(context.Background(), url, outDir, outFile, &DownloadOptions{Output: &mode}); res.Error != nil {
		t.Fatalf("Download: %v", res.Error)
	}
	if _, err := os.Stat(dl.getStateFilePath(la)); !os.IsNotExist(err) {
		t.Fatal("跑完之后状态文件不该还在")
	}
	// 切片临时目录也该空了（分片被挪进产物目录）
	segTemp := dl.segDir(la)
	if entries, err := os.ReadDir(segTemp); err == nil && len(entries) != 0 {
		t.Fatalf("收尾之后临时目录里不该还留着分片：%v", entries)
	}
}
