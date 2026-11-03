package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutDetectionAndLegacyUpgrade(t *testing.T) {
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, "go.mod"), "module demo\n")
	writeFileT(t, filepath.Join(root, "sites", "demo.go"), "package sites\n")
	writeFileT(t, filepath.Join(root, "docs", "README.md"), "legacy\n")
	if siteDirectory(root) != "sites" || frameworkDocsDir(root) != filepath.Join(root, "docs") {
		t.Fatal("legacy layout was not detected")
	}
	p, err := buildUpgradePlan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDocs(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "framework")); !os.IsNotExist(err) {
		t.Fatal("legacy upgrade must retain the existing docs layout")
	}
	writeFileT(t, filepath.Join(root, "internal", "sites", "demo.go"), "package sites\n")
	if siteDirectory(root) != filepath.Join("internal", "sites") {
		t.Fatal("internal declarations should take precedence over compatibility imports")
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
	// Simulate the legacy sites layout and compile its checker too.
	if err := os.Rename(filepath.Join("demo", "internal", "sites"), filepath.Join("demo", "sites")); err != nil {
		t.Fatal(err)
	}
	if err := runCheckCmd([]string{"demo"}); err != nil {
		t.Fatalf("legacy sites check: %v", err)
	}
}
