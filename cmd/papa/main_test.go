package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunNew 验证脚手架生成的文件齐全、内容正确。
func TestRunNew(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldwd)

	if err := runNew("demo"); err != nil {
		t.Fatalf("runNew: %v", err)
	}

	files := []string{
		"demo/go.mod",
		"demo/main.go",
		"demo/configs/config.yaml",
		"demo/fetcher/fetcher.go",
		"demo/models/content.go",
		"demo/logs/.gitkeep",
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing generated file %s: %v", f, err)
		}
	}

	if b, _ := os.ReadFile("demo/go.mod"); !strings.Contains(string(b), "module demo") {
		t.Errorf("go.mod 缺少 module 指令:\n%s", b)
	}
	if b, _ := os.ReadFile("demo/main.go"); !strings.Contains(string(b), `"github.com/ydtg1993/papa"`) {
		t.Errorf("main.go 缺少 papa import:\n%s", b)
	}
	if b, _ := os.ReadFile("demo/fetcher/fetcher.go"); !strings.Contains(string(b), "FetchHandler") || !strings.Contains(string(b), "GetStage") {
		t.Errorf("fetcher.go 缺少 Fetcher 接口方法")
	}
}

// TestScaffoldCompiles 是「可行性」的关键验证：生成一个项目，
// 用 replace 指向本地 papa，然后 go mod tidy + go build，证明生成的项目能编译通过。
// 依赖本地 Go 工具链和模块缓存，-short 时跳过。
func TestScaffoldCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping compile round-trip in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	// 从当前测试文件路径推导仓库根（cmd/papa -> 上两级）
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldwd)

	if err := runNew("demo"); err != nil {
		t.Fatalf("runNew: %v", err)
	}

	demoDir := filepath.Join(dir, "demo")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = demoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("go", "mod", "edit", "-replace", "github.com/ydtg1993/papa="+filepath.ToSlash(repoRoot))
	run("go", "mod", "tidy")
	run("go", "build", "./...")
}
