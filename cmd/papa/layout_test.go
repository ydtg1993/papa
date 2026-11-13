package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradeUsesOnlyV4Layout(t *testing.T) {
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, "go.mod"), "module demo\n")
	writeFileT(t, filepath.Join(root, "docs", "README.md"), "app-owned\n")
	p, err := buildUpgradePlan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDocs(p); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(root, "docs", "README.md")); got != "app-owned\n" {
		t.Fatal("overwrote application docs")
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "framework", "README.md")); err != nil {
		t.Fatal(err)
	}
	if data := skillData(root); data.CodeDir != "internal/" || data.DocsDir != "docs/framework" {
		t.Fatalf("non-v4 layout: %+v", data)
	}
}

func TestScaffoldBuildsAndUpgradePreservesAppDocs(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("GOWORK", "off")
	if err := runNew("demo", []string{"--replace", filepath.ToSlash(repo)}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = "demo"
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, output)
		}
	}
	for _, rel := range []string{"README.md", "docs/README.md", "docs/operations/DEVELOPMENT.md", "docs/business/custom.md"} {
		writeFileT(t, filepath.Join("demo", filepath.FromSlash(rel)), "app-owned\n")
	}
	writeFileT(t, filepath.Join("demo", "docs", "framework", "README.md"), "outdated\n")
	p, err := buildUpgradePlan("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDocs(p); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"README.md", "docs/README.md", "docs/operations/DEVELOPMENT.md", "docs/business/custom.md"} {
		if got := readFile(t, filepath.Join("demo", filepath.FromSlash(rel))); got != "app-owned\n" {
			t.Fatalf("upgrade overwrote %s", rel)
		}
	}
	if !strings.Contains(readFile(t, filepath.Join("demo", "main.go")), "app.Run(os.Args[1:])") {
		t.Fatal("root entry must delegate to app")
	}
	if _, err := os.Stat(filepath.Join("demo", "sites")); !os.IsNotExist(err) {
		t.Fatal("new scaffolds should not need a compatibility sites package")
	}
	if err := runCheckCmd([]string{"demo"}); err != nil {
		t.Fatalf("v4 sites check: %v", err)
	}
}
