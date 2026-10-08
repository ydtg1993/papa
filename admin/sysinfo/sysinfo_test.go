package sysinfo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 目录占用统计：递归累加所有文件，目录本身不计入。
func TestDirSize(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(p string, n int) {
		t.Helper()
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "a.bin"), 100)
	write(filepath.Join(sub, "b.bin"), 200)
	write(filepath.Join(sub, "deep", "c.bin"), 300)

	got, err := DirSize(root)
	if err != nil {
		t.Fatalf("DirSize = %v", err)
	}
	if got != 600 {
		t.Fatalf("DirSize = %d, want 600（递归累加）", got)
	}

	// 空目录 → 0
	empty := t.TempDir()
	if got, err := DirSize(empty); err != nil || got != 0 {
		t.Fatalf("空目录 = %d/%v, want 0/nil", got, err)
	}

	// 根目录不存在 → filepath.WalkDir 会把错误作为回调参数传进来，回调里吞掉，
	// 所以这里得到的是 0 而不是错误 —— 钉住它，免得有人以为"读不到就会报错"。
	if got, err := DirSize(filepath.Join(root, "nope")); err != nil || got != 0 {
		t.Fatalf("不存在的目录 = %d/%v, want 0/nil", got, err)
	}
}

// 单个文件（不是目录）也能被"统计"成它自己的大小 —— WalkDir 对文件也会回调一次。
// 这条同时说明 DirSize 的入参不做类型校验。
func TestDirSizeOnSingleFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.bin")
	if err := os.WriteFile(path, make([]byte, 42), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := DirSize(path); err != nil || got != 42 {
		t.Fatalf("DirSize(文件) = %d/%v, want 42/nil", got, err)
	}
}

// 目录采样把每个业务目录的占用写进快照（name -> {path, size}）。
func TestCollectorCollectDirs(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "x"), make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "y"), make([]byte, 20), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(time.Second, map[string]string{"logs": dirA, "data": dirB})
	c.collectDirs()

	snap := c.Snapshot()
	if len(snap.Dirs) != 2 {
		t.Fatalf("Dirs = %+v", snap.Dirs)
	}
	if got := snap.Dirs["logs"]; got.Size != 10 || got.Path != dirA {
		t.Fatalf("logs = %+v", got)
	}
	if got := snap.Dirs["data"]; got.Size != 20 {
		t.Fatalf("data = %+v", got)
	}
}

// 没配业务目录时 collectDirs 直接返回，且不把 Dirs 清成空 map（页面还是能读到上一次的值）。
func TestCollectorCollectDirsNoopWithoutDirs(t *testing.T) {
	c := NewCollector(time.Second, nil)
	c.collectDirs()
	if c.Snapshot().Dirs != nil {
		t.Fatalf("没配目录时不该写 Dirs：%+v", c.Snapshot().Dirs)
	}
}

// 目录不存在 → 记 0，不报错（业务目录可能是首次启动才创建的）。
func TestCollectorCollectDirsMissingPath(t *testing.T) {
	c := NewCollector(time.Second, map[string]string{"gone": filepath.Join(t.TempDir(), "nope")})
	c.collectDirs()

	got := c.Snapshot().Dirs["gone"]
	if got.Size != 0 {
		t.Fatalf("读不到的目录应记 0，实得 %d", got.Size)
	}
	if got.Path == "" {
		t.Fatal("路径仍应记下来（页面上要显示它在看哪儿）")
	}
}

// Snapshot 按值返回，但 Dirs 是 map —— 拷贝的是 map 头。
// 它之所以稳定，是因为 collectDirs **整体替换** map 而不是原地改元素。
// 这条把那个不变量钉住：一旦有人改成 `c.snapshot.Dirs[name] = ...`，
// 已经发出去的快照就会跟着变（监控页读到"同一份数据自己变了"）。
func TestCollectorSnapshotStaysStableAcrossSamples(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "x"), make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}

	c := NewCollector(time.Second, map[string]string{"logs": dirA})
	c.collectDirs()
	snap := c.Snapshot()

	// 换一批目录再采一次
	c.dirs = map[string]string{"logs": dirB}
	c.collectDirs()

	if len(snap.Dirs) != 1 || snap.Dirs["logs"].Path != dirA {
		t.Fatalf("采样应替换 map 而不是原地改，旧快照被污染了：%+v", snap.Dirs)
	}
	if now := c.Snapshot(); now.Dirs["logs"].Path != dirB {
		t.Fatalf("新快照应反映新目录：%+v", now.Dirs)
	}
}

// Start 起来先采一轮、并在 ctx 取消后干净退出。
func TestCollectorStartAndStop(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewCollector(time.Hour, map[string]string{"logs": dir})

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for c.Snapshot().Dirs == nil {
		if time.Now().After(deadline) {
			t.Fatal("Start 之后应当立刻采一轮目录占用")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.Snapshot().Dirs["logs"].Size; got != 7 {
		t.Fatalf("首轮采样的目录大小 = %d, want 7", got)
	}

	cancel()
	time.Sleep(30 * time.Millisecond) // 取消后协程应干净退出（不该 panic）
}
