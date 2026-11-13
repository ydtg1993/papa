package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/docs"
)

func TestScaffoldAndUpgradePreserveStructureManuals(t *testing.T) {
	manuals := []string{"ROOT_STRUCTURE.md", "ADMIN_STRUCTURE.md", "ENGINE_STRUCTURE.md", "INTERNAL_STRUCTURE.md", "PKG_STRUCTURE.md", "OBS_STRUCTURE.md", "CMD_STRUCTURE.md"}
	assertManuals := func(t *testing.T, dir string) {
		t.Helper()
		index := readFile(t, filepath.Join(dir, "package_structure", "README.md"))
		if !strings.Contains(readFile(t, filepath.Join(dir, "README.md")), "](package_structure/README.md)") {
			t.Fatal("framework navigation omitted the directory handbook")
		}
		for _, name := range manuals {
			rel := "package_structure/" + name
			want, err := docs.Files.ReadFile(rel)
			if err != nil {
				t.Fatal(err)
			}
			got := readFile(t, filepath.Join(dir, rel))
			if !sameContent([]byte(got), want) {
				t.Fatalf("framework manual missing or changed: %s", rel)
			}
			if !strings.Contains(index, "]("+name+")") {
				t.Fatalf("directory handbook does not link to %s", rel)
			}
		}
		for _, excluded := range []string{"issues", "superpowers"} {
			if _, err := os.Stat(filepath.Join(dir, excluded)); !os.IsNotExist(err) {
				t.Fatalf("repository-only history was distributed: %s (%v)", excluded, err)
			}
		}
	}
	t.Run("new-and-missing-only", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := runNew("demo", nil); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join("demo", "docs", "framework")
		assertManuals(t, dir)
		if _, err := os.Stat(filepath.Join(dir, "package_structure", "APP_STRUCTURE.md")); !os.IsNotExist(err) {
			t.Fatalf("new project still distributes the merged App manual: %v", err)
		}
		custom := filepath.Join(dir, "package_structure", "INTERNAL_STRUCTURE.md")
		writeFileT(t, custom, "local edits")
		if err := copyDocs(dir, true); err != nil {
			t.Fatal(err)
		}
		if readFile(t, custom) != "local edits" {
			t.Fatal("missing-only copy replaced local changes")
		}
	})
	for _, layout := range []string{"docs", "docs/framework"} {
		t.Run("upgrade-"+layout, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, layout)
			writeFileT(t, filepath.Join(dir, "CORE_CONFIG.md"), "old framework copy")
			writeFileT(t, filepath.Join(dir, "samples", "keep.md"), "business notes")
			adds, updates, stale := planDocs(root)
			if !contains(adds, "package_structure/INTERNAL_STRUCTURE.md") {
				t.Fatalf("upgrade omitted nested manuals: %v", adds)
			}
			if err := writeDocs(&upgradePlan{Root: root, DocAdds: adds, DocUpdates: updates, DocStale: stale}); err != nil {
				t.Fatal(err)
			}
			assertManuals(t, dir)
			if readFile(t, filepath.Join(dir, "samples", "keep.md")) != "business notes" {
				t.Fatal("upgrade replaced business notes")
			}
			adds, updates, _ = planDocs(root)
			if len(adds)+len(updates) != 0 {
				t.Fatalf("completed upgrade still has drift: adds=%v updates=%v", adds, updates)
			}
		})
	}
}
