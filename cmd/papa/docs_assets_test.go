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
