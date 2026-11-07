package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestNewReplacePathWithSpacesIsValidGoModule(t *testing.T) {
	t.Chdir(t.TempDir())
	if e := runNew("demo", []string{"--replace", "../local papa"}); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = "demo"
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("generated go.mod invalid: %v\n%s", e, out)
	}
}

func TestMissingOnlyAcceptsValidModuleDirectiveFormatting(t *testing.T) {
	for _, line := range []string{"module demo // business module", "module\tdemo", `module "demo"`} {
		t.Run(line, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if e := os.Mkdir("demo", 0755); e != nil {
				t.Fatal(e)
			}
			body := line + "\n\ngo 1.25.0\n"
			if e := os.WriteFile("demo/go.mod", []byte(body), 0600); e != nil {
				t.Fatal(e)
			}
			if e := runNew("demo", []string{"--missing-only"}); e != nil {
				t.Fatal(e)
			}
			if readFile(t, "demo/go.mod") != body {
				t.Fatal("existing module rewritten")
			}
		})
	}
}

func TestMissingOnlyConcurrentFileCreationNeverTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	a, b := strings.Repeat("A", 128<<10), strings.Repeat("B", 128<<10)
	var wg sync.WaitGroup
	for _, body := range []string{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := writeScaffoldFile(path, []byte(body), true); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	body, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if string(body) != a && string(body) != b {
		t.Fatal("concurrent scaffold produced partial or mixed content")
	}
}

func TestScaffoldRejectsWindowsJunctionWithoutWritingOutside(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows junction regression")
	}
	root, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(root, "configs")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path "+quote(link)+" -Target "+quote(outside)+" | Out-Null")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("create test junction: %v %s", e, out)
	}
	info, statErr := os.Lstat(link)
	if statErr != nil {
		t.Fatal(statErr)
	}
	t.Logf("junction mode=%s", info.Mode())
	// Remove only the junction entry before TempDir cleanup; never recurse into its target.
	t.Cleanup(func() {
		if e := os.Remove(link); e != nil {
			t.Error(e)
		}
	})
	if e := writeScaffoldFile(filepath.Join(link, "config.yaml"), []byte("private"), true); e == nil {
		t.Fatal("junction followed outside project")
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatal("junction target was changed", e)
	}
}

func TestNewRejectsMissingReplaceValueBeforeWriting(t *testing.T) {
	for _, args := range [][]string{{"--replace", "--missing-only"}, {"--replace="}, {"--replace", ""}} {
		t.Run(args[0]+filepath.Base(args[len(args)-1]), func(t *testing.T) {
			t.Chdir(t.TempDir())
			if e := runNew("demo", args); e == nil {
				t.Fatal("missing replace value accepted")
			}
			if _, e := os.Stat("demo"); !os.IsNotExist(e) {
				t.Fatal("invalid arguments left project files")
			}
		})
	}
}
