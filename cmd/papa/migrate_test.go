package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModulePath(t *testing.T) {
	cases := []struct {
		name, gomod, want string
	}{
		{"普通", "module demo\n\ngo 1.25.0\n", "demo"},
		{"带前导空格", "  module  demo/foo  \n", "demo/foo"},
		{"注释不算", "// module nope\nmodule real\n", "real"},
		{"没有 module 行", "go 1.25.0\n", ""},
		{"空文件", "", ""},
	}
	for _, c := range cases {
		if got := modulePath(c.gomod); got != c.want {
			t.Errorf("%s: modulePath = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBusinessProject(t *testing.T) {
	write := func(t *testing.T, gomod string) string {
		t.Helper()
		dir := t.TempDir()
		if gomod != "" {
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	// 业务项目：module 不是 papa，且依赖（require 或 replace）papa。
	dir := write(t, "module demo\n\ngo 1.25.0\n\nrequire github.com/ydtg1993/papa/v2 v2.5.3\n")
	if mod, ok := businessProject(dir); !ok || mod != "demo" {
		t.Errorf("业务项目应被认出，got %q/%v", mod, ok)
	}

	// 本地 replace 验证的场景：require + replace 都指向本地 papa。
	dir = write(t, "module hg\n\ngo 1.25.0\n\nrequire github.com/ydtg1993/papa/v2 v2.5.3\n\nreplace github.com/ydtg1993/papa/v2 => ../papa\n")
	if mod, ok := businessProject(dir); !ok || mod != "hg" {
		t.Errorf("replace 场景应被认出，got %q/%v", mod, ok)
	}

	// papa 仓库自己：即便文件名/路径像，也不该被当成业务项目。
	dir = write(t, "module "+papaModule+"\n\ngo 1.25.0\n")
	if mod, ok := businessProject(dir); ok {
		t.Errorf("papa 仓库自身不该被当作业务项目，got %q", mod)
	}

	// 无关的 Go 模块：不依赖 papa。
	dir = write(t, "module other\n\ngo 1.25.0\n\nrequire example.com/x v1.0.0\n")
	if mod, ok := businessProject(dir); ok {
		t.Errorf("不依赖 papa 的模块不该被当作业务项目，got %q", mod)
	}

	// 没有 go.mod。
	if mod, ok := businessProject(write(t, "")); ok {
		t.Errorf("没有 go.mod 时不该被当作业务项目，got %q", mod)
	}
}
