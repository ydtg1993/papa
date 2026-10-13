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

	tempDirPath := dl.segDir(la)
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
	tempDirPath := dl.segDir(la)
	if entries, err := os.ReadDir(tempDirPath); err == nil && len(entries) != 0 {
		t.Fatalf("KeepSegmentsAfterMerge=false 时收尾该清掉临时分片：%v", entries)
	}
}

// 状态文件里的"已完成分片"只当索引：偏移不在分片边界上（越界 / 不是 chunk_size 的整数倍）一律忽略。
//
// 合并是按偏移定位写的 —— 认下一个边界之外的值，就是往产物的任意位置塞字节；而分片循环也不会
// 为它生成任何一格。真正在场的那几片照旧合并出正确产物。
func TestResumeIgnoresOffBoundaryOffsets(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh"), 15) // 120 字节 → 三段 0/40/80
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
	segDir := dl.segDir(la)
	if err := os.MkdirAll(segDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 前两片真在盘上（内容取自 body），第三片留给这次跑去下
	for _, start := range []int64{0, 40} {
		if err := os.WriteFile(filepath.Join(segDir, fmt.Sprintf("chunk_%020d.tmp", start)), body[start:start+40], 0644); err != nil {
			t.Fatal(err)
		}
	}
	stale := &ResumeState{
		URL: url, OutputFile: name, TotalSize: int64(len(body)), ChunkSize: cfg.ChunkSize,
		// 后三个是边界之外的值：负数 · 不是 40 的整数倍 · 正好越过文件尾
		Completed: []int64{0, 40, -40, 55, int64(len(body))},
		TempFiles: []string{},
	}
	if err := dl.saveResumeState(la, stale); err != nil {
		t.Fatalf("造状态文件：%v", err)
	}

	res := dl.Download(context.Background(), url, outDir, name)
	if res.Error != nil {
		t.Fatalf("边界外的偏移只该被忽略，不该让整次下载失败：%v", res.Error)
	}
	got, err := os.ReadFile(filepath.Join(cfg.OutputDir, outDir, name))
	if err != nil {
		t.Fatalf("产物不在：%v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("产物内容不对（边界外的偏移被当成了写位置？）：%d 字节，want %d", len(got), len(body))
	}
}

// 状态文件里记的临时文件路径**不再被当成路径来源**：合并的路径一律由偏移拼出来。
//
// 旧写法按 `TempFiles` 给的路径 os.Open，偏移还是从**文件名**里 Sscanf 出来的 ——
// 状态被改过时，就能把进程读得到的任意文件合并进产物（这里用一个 tempDir 之外的诱饵文件演）。
func TestResumeDoesNotTrustStateFilePaths(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh"), 15)
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

	// 诱饵：长度与源文件一样，内容全是 Z，放在分片目录之外
	decoy := filepath.Join(tempDir(t), "decoy.bin")
	if err := os.WriteFile(decoy, bytes.Repeat([]byte("Z"), len(body)), 0644); err != nil {
		t.Fatal(err)
	}
	stale := &ResumeState{
		URL: url, OutputFile: name, TotalSize: int64(len(body)), ChunkSize: cfg.ChunkSize,
		Completed: []int64{0, 40, 80},
		TempFiles: []string{decoy, decoy, decoy}, // 三段都指着诱饵
	}
	if err := dl.saveResumeState(la, stale); err != nil {
		t.Fatalf("造状态文件：%v", err)
	}
	outPath := filepath.Join(cfg.OutputDir, outDir, name)

	res := dl.Download(context.Background(), url, outDir, name)
	if res.Error == nil {
		t.Fatal("状态说下好了、临时分片却不在：应当报错，而不是拿状态里的路径去合并")
	}
	if strings.Contains(res.Error.Error(), "decoy.bin") {
		t.Fatalf("报错里出现的应是拼出来的分片路径，不该是状态文件记的那条：%v", res.Error)
	}
	if !strings.Contains(res.Error.Error(), "open temp file") {
		t.Fatalf("报错该说清是分片缺失：%v", res.Error)
	}
	if got, err := os.ReadFile(outPath); err == nil && bytes.Contains(got, []byte("Z")) {
		t.Fatal("诱饵文件的内容被合并进了产物 —— 状态文件里的路径又被当成路径来源了")
	}

	// 状态已清掉 → 下一次跑从头重下
	if res2 := dl.Download(context.Background(), url, outDir, name); res2.Error != nil {
		t.Fatalf("清掉状态后重跑该成功：%v", res2.Error)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("产物不在：%v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("重下拿到的内容不对：%d 字节，want %d", len(got), len(body))
	}
}

// 状态说这一片下好了、文件也在，**但长度不对**（被截断）：必须报错并清状态，而不是照偏移
// 把那一片写进产物 —— 产物是先 Truncate(totalSize)、再按偏移一片片写的，短分片留下的那段是零，
// 而返回码是"成功"：事后只能靠人眼比对才发现（这是最难发现的一类坏）。
func TestResumeRejectsTruncatedSegment(t *testing.T) {
	body := bytes.Repeat([]byte("abcdefgh"), 15)
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
	segDir := dl.segDir(la)
	if err := os.MkdirAll(segDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 0 号完整；40 号被截断成 10 字节；80 号不在
	if err := os.WriteFile(filepath.Join(segDir, fmt.Sprintf("chunk_%020d.tmp", 0)), body[0:40], 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(segDir, fmt.Sprintf("chunk_%020d.tmp", 40)), body[40:50], 0644); err != nil {
		t.Fatal(err)
	}
	stale := &ResumeState{
		URL: url, OutputFile: name, TotalSize: int64(len(body)), ChunkSize: cfg.ChunkSize,
		Completed: []int64{0, 40, 80}, // 状态声称三片都下好了
		TempFiles: []string{},
	}
	if err := dl.saveResumeState(la, stale); err != nil {
		t.Fatalf("造状态文件：%v", err)
	}
	statePath := dl.getStateFilePath(la)

	res := dl.Download(context.Background(), url, outDir, name)
	if res.Error == nil {
		t.Fatal("分片长度不符时应当报错，而不是合并出一个带零填充空洞的产物")
	}
	if !strings.Contains(res.Error.Error(), "长度不符") {
		t.Fatalf("报错该说清是长度不符：%v", res.Error)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("报错时该顺手清掉状态文件，否则下一次跑还是栽在同一个地方")
	}

	// 自愈：再跑一次从头重下，产物必须与源逐字节一致
	if res2 := dl.Download(context.Background(), url, outDir, name); res2.Error != nil {
		t.Fatalf("清掉状态后重跑该成功：%v", res2.Error)
	}
	got, err := os.ReadFile(filepath.Join(cfg.OutputDir, outDir, name))
	if err != nil {
		t.Fatalf("产物不在：%v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("重下拿到的内容不对（第 40 片曾是被截断的）：%d 字节，want %d", len(got), len(body))
	}
}

// 分块大小配成 0 / 负数时会**永不退出**（`start += 0` 原地打转），兜底回落到默认 5MB：
// 与 MaxConcurrent / saveBatchSize 同一处理方式 —— 只补"会卡死"的那一类。
func TestChunkSizeNonPositiveFallsBackToDefault(t *testing.T) {
	if got := chunkSizeOrDefault(0); got != DefaultConfig().ChunkSize {
		t.Fatalf("0 应回落到默认值，实得 %d", got)
	}
	if got := chunkSizeOrDefault(-1); got != DefaultConfig().ChunkSize {
		t.Fatalf("负数应回落到默认值，实得 %d", got)
	}
	if got := chunkSizeOrDefault(4096); got != 4096 {
		t.Fatalf("正数应原样保留，实得 %d", got)
	}
}
