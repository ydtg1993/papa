package filedown

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 换源重写到同一个输出路径时，不能复用上一源的旧分片。
//
// 回归点：分片临时目录以前只按输出路径组织，分片跳过又只看磁盘大小、不认源 —— 同一路径先下 A 源、
// 再下 B 源，B 会复用 A 的分片，拼出混合内容还返回成功。现在分片目录带源哈希（见 segDir），
// 两个源各用各的目录，B 重新下、产物是 B 自己的内容。
func TestDownloadCrossSourceDoesNotReuseSegments(t *testing.T) {
	a := bytes.Repeat([]byte("A"), 30)
	b := bytes.Repeat([]byte("B"), 30) // 与 A 同长：分片大小检查会命中，正好触发旧代码的复用
	serverA := mockServer(a, true)
	serverB := mockServer(b, true)
	defer serverA.Close()
	defer serverB.Close()

	tempOut := tempDir(t)
	tempState := tempDir(t)
	cfg := &Config{
		OutputDir:              tempOut,
		ResumeStateDir:         tempState,
		ChunkSize:              10,
		EnableResume:           true,
		MaxConcurrent:          1,
		KeepSegmentsAfterMerge: true, // 让 A 的分片留在盘上，制造"上一源的分片还在"的场景
	}
	dl := NewDownloader(cfg)

	if res := dl.Download(context.Background(), serverA.URL, "sub", "file.bin"); res.Error != nil {
		t.Fatalf("下载 A：%v", res.Error)
	}
	if res := dl.Download(context.Background(), serverB.URL, "sub", "file.bin"); res.Error != nil {
		t.Fatalf("下载 B：%v", res.Error)
	}

	got, err := os.ReadFile(filepath.Join(tempOut, "sub", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, b) {
		t.Fatalf("产物应当是 B 的内容，实得 %q（复用了 A 的分片）", got)
	}
}

// 单文件降级下载失败时，不能毁掉上一份好文件，也不能留下半截临时文件。
//
// 回归点：downloadSingle 以前直接 os.Create(absOutput)，会**先截断**目标 —— 中途失败
// （读错 / 写错 / 长度不符）就留下半截文件，还把上一份好文件一起毁掉。现在写临时文件 + 成功后改名。
func TestDownloadSingleKeepsPreviousOnFailure(t *testing.T) {
	tempOut := tempDir(t)
	rel := filepath.Join("sub", "file.bin")
	abs := filepath.Join(tempOut, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("OLD-GOOD"), 0644); err != nil {
		t.Fatal(err)
	}

	// 服务器声明 100 字节、却只发 40 字节就断（Content-Length 与实发不符）
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 40))
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.OutputDir = tempOut
	dl := NewDownloader(cfg)

	if res := dl.Download(context.Background(), server.URL, "sub", "file.bin"); res.Error == nil {
		t.Fatal("短响应应当失败")
	}

	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "OLD-GOOD" {
		t.Fatalf("失败后上一份好文件被毁掉了，实得 %q", got)
	}
	if _, err := os.Stat(abs + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("失败的下载不该留下 .tmp")
	}
}

// 同键的 labor 引用计数：只要还有人排队，条目就不该删。
//
// 回归点：老写法"用完无条件 delete"会在 A 结束删掉条目后，让还排在旧锁上的 B 与新建的 C
// 各拿一条锁并发写同一个文件。引用计数把这个窗口关掉（见 acquireLabor / releaseLabor）。
func TestLaborRefcountKeepsEntryWhileWaiterExists(t *testing.T) {
	d := NewDownloader(DefaultConfig())
	key := "u|d|f"

	l1 := d.acquireLabor(key, "u", "d", "f")
	l2 := d.acquireLabor(key, "u", "d", "f")
	if l1 != l2 {
		t.Fatal("同键应返回同一条 labor")
	}

	d.releaseLabor(key, l1)
	d.st.laborMu.RLock()
	_, still := d.st.labors[key]
	d.st.laborMu.RUnlock()
	if !still {
		t.Fatal("还有一个 waiter 没走，条目不该删")
	}

	d.releaseLabor(key, l2)
	d.st.laborMu.RLock()
	_, still = d.st.labors[key]
	d.st.laborMu.RUnlock()
	if still {
		t.Fatal("最后一个 waiter 走后条目该删掉")
	}
}
