package main

import (
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/docs"
)

func TestScaffoldAndUpgradePreserveStructureManuals(t *testing.T) {
	manuals := []string{"ROOT_STRUCTURE.md", "ADMIN_STRUCTURE.md", "ENGINE_STRUCTURE.md", "INTERNAL_STRUCTURE.md", "PKG_STRUCTURE.md", "OBS_STRUCTURE.md", "CMD_STRUCTURE.md"}
	assertManuals := func(t *testing.T, dir string) {
		t.Helper()
		for _, rel := range []string{
			"architecture/README.md", "architecture/ARCHITECTURE_DIAGRAMS.md",
			"configuration/README.md", "configuration/CORE_CONFIG.md",
			"guides/README.md", "guides/FETCHER_WRITING_GUIDE.md",
			"accounts/README.md", "accounts/ACCOUNT_RUNTIME.md",
			"runtime/README.md", "runtime/RECOVER_QUEUE.md",
			"operations/README.md", "operations/CLI.md",
		} {
			if _, err := docs.Files.ReadFile(rel); err != nil {
				t.Fatalf("missing current manual: %s: %v", rel, err)
			}
		}
		for _, rel := range embeddedDocs() {
			want, err := docs.Files.ReadFile(rel)
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(dir, filepath.FromSlash(rel))); !sameContent([]byte(got), want) {
				t.Fatalf("distributed content differs: %s", rel)
			}
		}
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
		for _, excluded := range []string{"history", "issues", "superpowers", "CORE_CONFIG.md", "CLI.md"} {
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
	for _, layout := range []string{"docs/framework"} {
		t.Run("upgrade-"+layout, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, layout)
			writeFileT(t, filepath.Join(dir, "configuration/CORE_CONFIG.md"), "old framework copy")
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
			// A previous flat copy may contain local edits. Refreshing the new
			// topic path must not become implicit authorization to delete it.
			oldPath := filepath.Join(dir, "CORE_CONFIG.md")
			writeFileT(t, oldPath, "previous flat copy with local notes")
			adds, updates, stale = planDocs(root)
			if !contains(stale, "CORE_CONFIG.md") {
				t.Fatalf("old copy was not reported for owner review: %v", stale)
			}
			if err := writeDocs(&upgradePlan{Root: root, DocAdds: adds, DocUpdates: updates, DocStale: stale}); err != nil {
				t.Fatal(err)
			}
			if readFile(t, oldPath) != "previous flat copy with local notes" {
				t.Fatal("upgrade altered the previous flat copy")
			}
		})
	}
}

// Links must resolve inside the distributed filesystem, not merely in the
// repository: history exists in the checkout but is deliberately not shipped.
func TestEmbeddedManualLinksAndNavigation(t *testing.T) {
	links := regexp.MustCompile(`\[[^\]\n]*\]\(([^)\n]+)\)`)
	fences := regexp.MustCompile("(?ms)^```[^\\n]*\\n.*?^```[ \\t]*$")
	visited := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		body, err := docs.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range links.FindAllStringSubmatch(fences.ReplaceAllString(string(body), ""), -1) {
			u, err := url.Parse(match[1])
			if err != nil {
				t.Fatalf("%s: invalid link %q: %v", name, match[1], err)
			}
			if u.IsAbs() || u.Host != "" || u.Path == "" {
				continue
			}
			target := path.Clean(path.Join(path.Dir(name), u.Path))
			if _, err := fs.Stat(docs.Files, target); err != nil {
				t.Errorf("%s: undistributed or missing link %q: %v", name, match[1], err)
				continue
			}
			if strings.HasSuffix(target, ".md") {
				walk(target)
			}
		}
	}
	walk("README.md")
	for _, name := range embeddedDocs() {
		if strings.HasPrefix(name, "history/") {
			t.Errorf("historical material was embedded: %s", name)
		}
		if !visited[name] {
			t.Errorf("manual is unreachable from README: %s", name)
		}
	}
}
