package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	if bytes.Contains(raw, []byte("\r\n")) {
		t.Fatal("diagram manifest must use LF to match checkout rules")
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Diagrams) != 9 {
		t.Fatalf("expected six overview and three engine diagrams, got %d", len(manifest.Diagrams))
	}
	root := t.TempDir()
	dir := frameworkDocsDir(root)
	if err := copyDocs(dir, false); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, diagram := range manifest.Diagrams {
		if seen[diagram.Name] {
			t.Fatalf("duplicate diagram in manifest: %s", diagram.Name)
		}
		seen[diagram.Name] = true
		for ext, expectedHash := range map[string]string{"mmd": diagram.SourceSHA256, "svg": diagram.SVGsha256, "png": diagram.PNGsha256} {
			name := "assets/diagrams/" + diagram.Name + "." + ext
			want, err := docs.Files.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if ext != "png" && bytes.Contains(want, []byte("\r\n")) {
				t.Fatalf("diagram text must use LF to match checkout rules: %s", name)
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
	for _, name := range embeddedDocs() {
		if strings.HasPrefix(name, "assets/diagrams/") && strings.HasSuffix(name, ".mmd") && !seen[strings.TrimSuffix(filepath.Base(name), ".mmd")] {
			t.Fatalf("diagram missing from manifest: %s", name)
		}
	}
	// Exercise replacement of every diagram, including the Engine additions.
	// Checking only the first image would miss a partial upgrade of later assets.
	damaged := map[string][]byte{}
	for _, diagram := range manifest.Diagrams {
		name := "assets/diagrams/" + diagram.Name + ".png"
		want, err := docs.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		corrupted := bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
		if bytes.Equal(corrupted, want) {
			t.Fatalf("fixture must change PNG bytes: %s", name)
		}
		damaged[name] = corrupted
	}
	for _, name := range []string{"architecture/ENGINE_GUIDE.md", "assets/diagrams/manifest.json", "assets/diagrams/07-engine-components.mmd", "assets/diagrams/08-engine-scheduling.svg"} {
		damaged[name] = []byte("outdated framework copy")
	}
	for name, corrupted := range damaged {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), corrupted, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyDocs(dir, true); err != nil {
		t.Fatal(err)
	}
	for name, corrupted := range damaged {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, corrupted) {
			t.Fatalf("missing-only changed existing resource %s: %v", name, err)
		}
	}
	adds, updates, stale := planDocs(root)
	for name := range damaged {
		if !contains(updates, name) {
			t.Fatalf("upgrade omitted changed resource: %s", name)
		}
	}
	if err := writeDocs(&upgradePlan{Root: root, DocAdds: adds, DocUpdates: updates, DocStale: stale}); err != nil {
		t.Fatal(err)
	}
	for name := range damaged {
		want, err := docs.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade did not restore exact resource %s: %v", name, err)
		}
	}
	adds, updates, _ = planDocs(root)
	if len(adds)+len(updates) != 0 {
		t.Fatalf("asset upgrade is not idempotent: %v %v", adds, updates)
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
