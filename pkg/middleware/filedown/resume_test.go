package filedown

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 断点状态与磁盘对不上（状态说"分片都下好了"、临时分片却不在）时：
// 必须**清掉状态文件再报错**，让下一次跑从头重下。
//
// 这正是"先删分片、后清状态"那个顺序留下的残局：进程死在这两步之间，状态文件还在、
// 分片已经没了。旧写法每次跑都跳过所有下载直接进合并 → 撞 open temp file ... no such file →
// 失败在清理那一步之前返回 → 状态文件照旧 → 这个任务**永久失败**，只能人工去删状态文件。
func TestDownloadSelfHealsStaleResumeState(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeContent 会按需回 HEAD / Content-Length / Range —— filedown 探测与分片都靠它
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.ChunkSize = 40 // 100 字节 → 三段（0/40/80），与下面手造的状态对齐
	cfg.MaxConcurrent = 1
	dl := NewDownloader(cfg)

	const outDir, name = "sub", "file.bin"
	url := server.URL + "/file.bin"
	la := &labor{url: url, outputDir: outDir, filename: name}

	tempDirPath := filepath.Join(cfg.ResumeStateDir, "segments", outDir, name)
	// 手工造一份"三段都下好了"的状态；临时分片一个都不建 —— 就是残局那一刻的样子
	stale := &ResumeState{
		URL:        url,
		OutputFile: name,
		TotalSize:  int64(len(body)),
		ChunkSize:  cfg.ChunkSize,
		Completed:  []int64{0, 40, 80},
		TempFiles: []string{
			filepath.Join(tempDirPath, fmt.Sprintf("chunk_%020d.tmp", 0)),
			filepath.Join(tempDirPath, fmt.Sprintf("chunk_%020d.tmp", 40)),
			filepath.Join(tempDirPath, fmt.Sprintf("chunk_%020d.tmp", 80)),
		},
	}
	if err := dl.saveResumeState(la, stale); err != nil {
		t.Fatalf("造状态文件：%v", err)
	}
	statePath := dl.getStateFilePath(la)

	res := dl.Download(context.Background(), url, outDir, name)
	if res.Error == nil {
		t.Fatal("临时分片缺失时应当报错，而不是硬着头皮去合并")
	}
	if !strings.Contains(res.Error.Error(), "open temp file") {
		t.Fatalf("报错该说清是分片缺失，实得：%v", res.Error)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("报错时该顺手清掉状态文件，否则下一次跑还是栽在同一个地方")
	}

	// 自愈的验收：再跑一次必须真去下载（而不是继续跳过下载、继续撞合并）
	res2 := dl.Download(context.Background(), url, outDir, name)
	if res2.Error != nil {
		t.Fatalf("清掉状态后重跑该从头下成功，实得：%v", res2.Error)
	}
	got, err := os.ReadFile(filepath.Join(cfg.OutputDir, outDir, name))
	if err != nil {
		t.Fatalf("产物不在：%v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("重下拿到的内容不对：%d 字节，want %d", len(got), len(body))
	}
}

// 正常跑完之后状态文件与临时分片都不该还在（收尾两步都做到了）。
func TestDownloadCleansStateAndTempAfterSuccess(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempDir(t)
	cfg.ResumeStateDir = tempDir(t)
	cfg.ChunkSize = 40
	cfg.MaxConcurrent = 1
	dl := NewDownloader(cfg)

	const outDir, name = "sub", "file.bin"
	url := server.URL + "/file.bin"
	la := &labor{url: url, outputDir: outDir, filename: name}

	if res := dl.Download(context.Background(), url, outDir, name); res.Error != nil {
		t.Fatalf("Download: %v", res.Error)
	}
	if _, err := os.Stat(dl.getStateFilePath(la)); !os.IsNotExist(err) {
		t.Fatal("跑完之后状态文件不该还在")
	}
	tempDirPath := filepath.Join(cfg.ResumeStateDir, "segments", outDir, name)
	if entries, err := os.ReadDir(tempDirPath); err == nil && len(entries) != 0 {
		t.Fatalf("KeepSegmentsAfterMerge=false 时收尾该清掉临时分片：%v", entries)
	}
}
