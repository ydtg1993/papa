package main

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestUpgradeDoesNotModifyHardLinkedBusinessFiles(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatal(err)
	}
	links := []string{"docs/framework/README.md", "docs/framework/assets/diagrams/01-capabilities.png", skillPath(t, "SKILL.md")}
	for i, rel := range links {
		dst := filepath.Join("demo", rel)
		business := filepath.Join("demo", "business", filepath.Base(rel))
		writeFileT(t, business, "business-owned bytes "+string(rune('A'+i)))
		if err := os.Remove(dst); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(business, dst); err != nil {
			t.Fatal(err)
		}
	}
	before := snapshotProjectFiles(t, "demo")
	if err := runUpgradeCmd([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	assertProjectSnapshot(t, "demo", before)
	if err := runUpgradeCmd([]string{"demo", "--write"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range links {
		business := filepath.Join("business", filepath.Base(rel))
		if got := readFile(t, filepath.Join("demo", business)); got != string(before[business]) {
			t.Errorf("upgrade modified hard-linked business file: %s", business)
		}
	}
	adds, updates, _ := planDocs("demo")
	refresh, seed, _ := planSkill("demo")
	if len(adds)+len(updates)+len(refresh)+len(seed) != 0 {
		t.Fatal("upgrade did not publish all framework copies")
	}
	after := snapshotProjectFiles(t, "demo")
	for name := range after {
		if strings.Contains(filepath.Base(name), ".papa-refresh-") {
			t.Fatalf("temporary refresh file leaked: %s", name)
		}
	}
}

func snapshotProjectFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertProjectSnapshot(t *testing.T, root string, want map[string][]byte) {
	t.Helper()
	got := snapshotProjectFiles(t, root)
	if len(got) != len(want) {
		t.Fatalf("project file set changed: %d -> %d", len(want), len(got))
	}
	for name, data := range want {
		if !bytes.Equal(got[name], data) {
			t.Errorf("project file changed: %s", name)
		}
	}
}

func TestCommandsRejectNestedLinksWithoutChangingTargets(t *testing.T) {
	for _, command := range []string{"new", "upgrade"} {
		for _, rel := range []string{"docs/framework/assets", ".claude/skills/papa-crawler-development/references"} {
			t.Run(command+"/"+rel, func(t *testing.T) {
				t.Chdir(t.TempDir())
				writeFileT(t, "demo/go.mod", "module demo\n\ngo 1.25.0\n")
				writeFileT(t, "demo/main.go", "business source must survive")
				outside := t.TempDir()
				writeFileT(t, filepath.Join(outside, "sentinel.bin"), "business bytes")
				before := snapshotProjectFiles(t, outside)
				link := filepath.Join("demo", rel)
				if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
					t.Fatal(err)
				}
				testDocDirectoryLink(t, link, outside)
				var err error
				if command == "new" {
					err = runNew("demo", []string{"--missing-only"})
				} else {
					err = runUpgradeCmd([]string{"demo", "--write"})
				}
				if err == nil || !strings.Contains(err.Error(), "包含链接") {
					t.Fatalf("expected linked-path rejection: %v", err)
				}
				assertProjectSnapshot(t, outside, before)
				if readFile(t, "demo/main.go") != "business source must survive" {
					t.Fatal("business source changed")
				}
			})
		}
	}
}

func TestUpgradeRejectsFileLinksAndNonRegularTargets(t *testing.T) {
	for _, kind := range []string{"file-link", "dangling-link", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			dst := filepath.Join(frameworkDocsDir(root), "README.md")
			if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(outside, "business.md")
			if kind == "file-link" {
				writeFileT(t, target, "business content")
			}
			if kind == "directory" {
				if err := os.Mkdir(dst, 0755); err != nil {
					t.Fatal(err)
				}
				writeFileT(t, filepath.Join(dst, "keep.md"), "directory contents")
			} else if err := os.Symlink(target, dst); err != nil {
				t.Skipf("file symlink unavailable: %v", err)
			}
			if kind != "directory" {
				t.Cleanup(func() {
					if err := os.Remove(dst); err != nil {
						t.Error(err)
					}
				})
			}
			before := snapshotProjectFiles(t, outside)
			err := writeDocs(&upgradePlan{Root: root, DocUpdates: []string{"README.md"}})
			if err == nil {
				t.Fatal("unsafe target accepted")
			}
			assertProjectSnapshot(t, outside, before)
			if kind == "directory" && readFile(t, filepath.Join(dst, "keep.md")) != "directory contents" {
				t.Fatal("target directory changed")
			}
			entries, err := os.ReadDir(filepath.Dir(dst))
			if err != nil || len(entries) != 1 {
				t.Fatalf("refresh left extra files: %v %v", entries, err)
			}
		})
	}
}

func TestUpgradePreservesEveryProjectOwnedFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatal(err)
	}
	owned := map[string]bool{}
	for _, name := range embeddedDocs() {
		owned[filepath.Join("docs", "framework", filepath.FromSlash(name))] = true
	}
	for _, sf := range skillFiles() {
		if sf.refreshed {
			owned[sf.rel] = true
		}
	}
	for name := range snapshotProjectFiles(t, "demo") {
		if !owned[name] && name != "go.mod" {
			writeFileT(t, filepath.Join("demo", name), "custom business content: "+name)
		}
	}
	for _, name := range []string{"docs/CLI.md", "docs/business/README.md", "docs/framework/custom/note.md", "docs/framework/custom/image.png", ".claude/skills/custom/SKILL.md"} {
		writeFileT(t, filepath.Join("demo", name), "unregistered project content")
	}
	writeFileT(t, "demo/docs/framework/README.md", "old framework copy")
	writeFileT(t, filepath.Join("demo", skillPath(t, "SKILL.md")), "old common skill")
	before := snapshotProjectFiles(t, "demo")
	if err := runNew("demo", nil); err == nil {
		t.Fatal("new accepted an existing business project")
	}
	assertProjectSnapshot(t, "demo", before)
	if err := runNew("demo", []string{"--missing-only"}); err != nil {
		t.Fatal(err)
	}
	assertProjectSnapshot(t, "demo", before)
	if err := runUpgradeCmd([]string{"demo"}); err != nil {
		t.Fatal(err)
	}
	assertProjectSnapshot(t, "demo", before)
	if err := runUpgradeCmd([]string{"demo", "--write"}); err != nil {
		t.Fatal(err)
	}
	after := snapshotProjectFiles(t, "demo")
	if len(before) != len(after) {
		t.Fatal("upgrade added/deleted unexpected files")
	}
	for name, data := range before {
		if !owned[name] && !bytes.Equal(after[name], data) {
			t.Errorf("project-owned file changed: %s", name)
		}
	}
	adds, updates, _ := planDocs("demo")
	refresh, seed, _ := planSkill("demo")
	if len(adds)+len(updates)+len(refresh)+len(seed) != 0 {
		t.Fatal("framework refresh was not completed")
	}
	if err := runUpgradeCmd([]string{"demo", "--write"}); err != nil {
		t.Fatal(err)
	}
	assertProjectSnapshot(t, "demo", after)
}

func TestFrameworkRefreshFailurePreservesOriginalAndCleansTemp(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows open handle blocks replacement")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "README.md")
	writeFileT(t, dst, "old complete file")
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := replaceScaffoldFile(dst, []byte("new complete file")); err == nil {
		t.Fatal("fixture must block replacement while the old file is open")
	}
	if readFile(t, dst) != "old complete file" {
		t.Fatal("failed refresh damaged the previous file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "README.md" {
		t.Fatalf("temporary file leaked: %v %v", entries, err)
	}
}

func testDocDirectoryLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path "+quote(link)+" -Target "+quote(target)+" | Out-Null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create test junction: %v %s", err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Only remove the link entry; never recurse into the external target.
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil {
			t.Error(err)
		}
	})
}

func TestUpgradeRejectsLinkedRootsBeforeCreatingDirectories(t *testing.T) {
	for _, which := range []string{"docs", "skills"} {
		t.Run(which, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			var err error
			if which == "docs" {
				testDocDirectoryLink(t, filepath.Join(root, "docs"), outside)
				err = writeDocs(&upgradePlan{Root: root, DocAdds: []string{"README.md"}})
			} else {
				testDocDirectoryLink(t, filepath.Join(root, ".claude"), outside)
				refresh, seed, _ := planSkill(root)
				err = writeSkill(&upgradePlan{Root: root, SkillRefresh: refresh, SkillSeed: seed})
			}
			if err == nil {
				t.Error("upgrade followed a directory link")
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("external target changed: %v %v", entries, readErr)
			}
		})
	}
}

func TestUpgradePreservesProjectConventionsCreatedAfterPlan(t *testing.T) {
	root := t.TempDir()
	refresh, seed, _ := planSkill(root)
	var owned string
	for _, item := range skillFiles() {
		if !item.refreshed {
			owned = filepath.Join(root, item.rel)
		}
	}
	if owned == "" {
		t.Fatal("missing project-owned skill fixture")
	}
	writeFileT(t, owned, "project notes created after planning")
	if err := writeSkill(&upgradePlan{Root: root, SkillRefresh: refresh, SkillSeed: seed}); err != nil {
		t.Fatal(err)
	}
	if readFile(t, owned) != "project notes created after planning" {
		t.Fatal("upgrade overwrote newly created project conventions")
	}
}
