package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestV4ModuleAndScaffoldIdentity(t *testing.T) {
	const want = "github.com/ydtg1993/papa/v4"
	b, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if modulePath(string(b)) != want || papaModule != want {
		t.Fatal("framework and CLI must declare the v4 module")
	}
	err = filepath.WalkDir("templates", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "github.com/ydtg1993/papa/"+"v3") {
			t.Errorf("legacy import in scaffold %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestV4CLIRejectsLegacyBeforeWork(t *testing.T) {
	root := t.TempDir()
	legacy := "github.com/ydtg1993/papa/" + "v3"
	for _, declaration := range []string{"require " + legacy + " v3.8.10", "require (\n" + legacy + " v3.8.10\n)", "replace " + legacy + " => ../papa"} {
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module legacy\n\ngo 1.25.0\n"+declaration+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(root)
		for _, call := range []func() error{func() error { return runMigrateCmd(nil) }, func() error { return runCheckCmd(nil) }, func() error { return runUpgradeCmd([]string{"--write"}) }} {
			if err := call(); err == nil || !strings.Contains(err.Error(), "/v3") {
				t.Fatalf("legacy project not rejected: %v", err)
			}
		}
		if _, err := os.Stat("docs"); !os.IsNotExist(err) {
			t.Fatal("wrote v4 docs to legacy project")
		}
	}
	if err := os.WriteFile("go.mod", []byte("module demo\n// "+legacy+"\nrequire "+papaModule+" v4.2.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateProjectModule("."); err != nil {
		t.Fatal(err)
	}
}

func TestV4ModuleValidationRejectsAllEarlierModules(t *testing.T) {
	root := t.TempDir()
	for _, module := range []string{"github.com/ydtg1993/papa", "github.com/ydtg1993/papa/v1", "github.com/ydtg1993/papa/v2", "github.com/ydtg1993/papa/v3"} {
		for _, declaration := range []string{"require " + module + " v1.0.0", "replace (\n" + papaModule + " => " + module + " v1.0.0\n)", "module " + module} {
			writeFileT(t, filepath.Join(root, "go.mod"), declaration+"\n")
			if err := validateProjectModule(root); err == nil {
				t.Fatalf("accepted unsupported module: %s", declaration)
			}
		}
	}
	writeFileT(t, filepath.Join(root, "go.mod"), "module example.com/papa/v3\n// github.com/ydtg1993/papa/v3\nrequire "+papaModule+" v4.0.0\n")
	if err := validateProjectModule(root); err != nil {
		t.Fatal(err)
	}
}

func TestV4NewMissingOnlyRejectsEarlierProject(t *testing.T) {
	t.Chdir(t.TempDir())
	mod := "module demo\nrequire github.com/ydtg1993/papa/v3 v3.8.10\n"
	writeFileT(t, filepath.Join("demo", "go.mod"), mod)
	if err := runNew("demo", []string{"--missing-only"}); err == nil {
		t.Fatal("accepted an earlier project for v4 scaffold completion")
	}
	entries, err := os.ReadDir("demo")
	if err != nil || len(entries) != 1 || readFile(t, filepath.Join("demo", "go.mod")) != mod {
		t.Fatalf("rejected project was modified: %v, %v", entries, err)
	}
}
