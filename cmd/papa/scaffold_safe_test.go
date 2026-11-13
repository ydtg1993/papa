package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRejectsExistingProjectAndMissingOnlyPreservesBusiness(t *testing.T) {
	t.Chdir(t.TempDir())
	if e := os.Mkdir("demo", 0755); e != nil {
		t.Fatal(e)
	}
	original := map[string]string{"go.mod": "module demo\n\ngo 1.25.0\n", "main.go": "package main\n// hand written\n", "configs/config.yaml": "private config\n", "docs/framework/operations/CLI.md": "custom copy\n"}
	for path, body := range original {
		p := filepath.Join("demo", path)
		_ = os.MkdirAll(filepath.Dir(p), 0755)
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := runNew("demo", nil); e == nil || !strings.Contains(e.Error(), "拒绝覆盖") {
		t.Fatal(e)
	}
	if _, e := os.Stat("demo/Makefile"); !os.IsNotExist(e) {
		t.Fatal("default mutated existing project")
	}
	if e := runNew("demo", []string{"--missing-only", "--replace", "/must-not-rewrite-module"}); e != nil {
		t.Fatal(e)
	}
	for path, body := range original {
		if readFile(t, filepath.Join("demo", path)) != body {
			t.Fatal("overwritten", path)
		}
	}
	if _, e := os.Stat("demo/internal/app/run.go"); e != nil {
		t.Fatal(e)
	}
}
func TestNewMissingOnlyRejectsModuleMismatchAndLinkedPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	_ = os.Mkdir("demo", 0755)
	_ = os.WriteFile("demo/go.mod", []byte("module another\n"), 0600)
	if e := runNew("demo", []string{"--missing-only"}); e == nil {
		t.Fatal("module mismatch accepted")
	}
	_ = os.WriteFile("demo/go.mod", []byte("module demo\n"), 0600)
	outside := t.TempDir()
	if e := os.Symlink(outside, "demo/configs"); e != nil {
		t.Skip("symlink privilege unavailable")
	}
	if e := runNew("demo", []string{"--missing-only"}); e == nil {
		t.Fatal("symlink followed")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) > 0 {
		t.Fatal("wrote outside project")
	}
}
