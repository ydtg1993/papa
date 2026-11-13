package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/docs"
)

func TestDiagramAssetsArePublishedAndUpgradedExactly(t *testing.T) {
	var manifest struct {
		Diagrams []struct{ Name, SourceSHA256, SVGsha256, PNGsha256 string }
	}
	raw, err := docs.Files.ReadFile("assets/diagrams/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Diagrams) != 6 {
		t.Fatalf("expected six introductory diagrams, got %d", len(manifest.Diagrams))
	}
	root := t.TempDir()
	dir := frameworkDocsDir(root)
	if err := copyDocs(dir, false); err != nil {
		t.Fatal(err)
	}
	for _, diagram := range manifest.Diagrams {
		for ext, expectedHash := range map[string]string{"mmd": diagram.SourceSHA256, "svg": diagram.SVGsha256, "png": diagram.PNGsha256} {
			name := "assets/diagrams/" + diagram.Name + "." + ext
			want, err := docs.Files.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(want)
			if hex.EncodeToString(hash[:]) != expectedHash {
				t.Fatalf("diagram source/output manifest is stale: %s", name)
			}
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("asset was not copied exactly: %s: %v", name, err)
			}
		}
	}
	name := "assets/diagrams/01-capabilities.png"
	want, _ := docs.Files.ReadFile(name)
	corrupted := bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if bytes.Equal(corrupted, want) {
		t.Fatal("fixture must change PNG bytes")
	}
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.WriteFile(file, corrupted, 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyDocs(dir, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file)
	if !bytes.Equal(got, corrupted) {
		t.Fatal("missing-only replaced an existing image")
	}
	adds, updates, stale := planDocs(root)
	if !contains(updates, name) {
		t.Fatal("binary image drift was hidden by newline normalization")
	}
	if err := writeDocs(&upgradePlan{Root: root, DocAdds: adds, DocUpdates: updates, DocStale: stale}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(file)
	if !bytes.Equal(got, want) {
		t.Fatal("upgrade did not restore the exact PNG")
	}
	adds, updates, _ = planDocs(root)
	if len(adds)+len(updates) != 0 {
		t.Fatalf("asset upgrade is not idempotent: %v %v", adds, updates)
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

func TestGeneratedDocumentationAndSkillPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatal(err)
	}
	links := regexp.MustCompile(`\[[^\]\n]*\]\(([^)\n]+)\)`)
	fences := regexp.MustCompile("(?ms)^```[^\\n]*\\n.*?^```[ \\t]*$")
	checkoutRefs := regexp.MustCompile(`papa/docs/([A-Za-z0-9_./-]+\.md)`)
	projectRefs := regexp.MustCompile(`docs/framework/([A-Za-z0-9_./-]+\.md)`)
	err := filepath.Walk("demo", func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(name) != ".md" {
			return nil
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		body := string(raw)
		for _, ref := range checkoutRefs.FindAllStringSubmatch(body, -1) {
			if _, err := docs.Files.ReadFile(ref[1]); err != nil {
				t.Errorf("%s: invalid skill checkout reference %s", name, ref[0])
			}
		}
		for _, ref := range projectRefs.FindAllStringSubmatch(body, -1) {
			if _, err := os.Stat(filepath.Join("demo", filepath.FromSlash(ref[0]))); err != nil {
				t.Errorf("%s: invalid project documentation reference %s", name, ref[0])
			}
		}
		for _, ref := range links.FindAllStringSubmatch(fences.ReplaceAllString(body, ""), -1) {
			u, err := url.Parse(ref[1])
			if err != nil {
				t.Errorf("%s: invalid link %s", name, ref[1])
				continue
			}
			if u.IsAbs() || u.Host != "" {
				continue
			}
			target := filepath.Join(filepath.Dir(name), filepath.FromSlash(u.Path))
			if u.Path == "" {
				target = name
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: missing generated link %s", name, ref[1])
				continue
			}
			if u.Fragment != "" && filepath.Ext(target) == ".md" {
				data, err := os.ReadFile(target)
				if err != nil {
					return err
				}
				if !manualAnchors(string(data))[u.Fragment] {
					t.Errorf("%s: missing generated anchor %s", name, ref[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
