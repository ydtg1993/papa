package server

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ydtg1993/oao"
)

// Exercise Papa against the assets embedded in the linked release, rather
// than assuming the adjacent oao checkout matches the published dependency.
func TestMonitorLinkedVisualsBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for UI behavior tests")
	}
	assets, err := new(oao.Oao).StaticFS()
	if err != nil {
		t.Fatal(err)
	}
	if !hasVisualAssets(assets, "oao-charts") || !hasVisualAssets(assets, "oao-motion") {
		t.Skip("linked oao does not include optional chart and motion assets")
	}
	dir := t.TempDir()
	for name, source := range map[string]fs.FS{
		"oao-charts.js": assets, "oao-motion.js": assets,
		"static/workbench.js": staticFS, "static/visual-state.js": staticFS, "static/visuals.js": staticFS,
	} {
		b, err := fs.ReadFile(source, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(name)), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, node, filepath.Join("testdata", "linked_visuals_test.js"), dir).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
}

func TestMonitorOptionalVisualReferencesMatchLinkedDependency(t *testing.T) {
	response := serve(newFakeMonitor(t), http.MethodGet, "/monitor", nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	charts, motion := optionalOaoAssets()
	for name, present := range map[string]bool{"oao-charts": charts, "oao-motion": motion} {
		for _, ext := range []string{".js", ".css"} {
			if strings.Contains(response.Body.String(), "/static/oao/"+name+ext) != present {
				t.Fatalf("optional %s%s referenced without matching linked capability", name, ext)
			}
		}
	}
}

func TestOptionalVisualAssetsRequireCompletePair(t *testing.T) {
	assets := fstest.MapFS{"oao-charts.js": {Data: []byte("chart")}}
	if hasVisualAssets(assets, "oao-charts") {
		t.Fatal("missing stylesheet must not enable charts")
	}
	assets["oao-charts.css"] = &fstest.MapFile{Data: []byte("style")}
	if !hasVisualAssets(assets, "oao-charts") || hasVisualAssets(assets, "oao-motion") {
		t.Fatal("optional modules must be detected independently")
	}
}

func TestMonitorReducedMotionDisablesFeedbackTransforms(t *testing.T) {
	b, err := staticFS.ReadFile("static/visuals.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	feedback := ":is(.nav-item,.theme-toggle,.site-tab,.card,.tile,.btn,.pager .pg,.bell):is(:hover,:active) { transform:none !important; }"
	if !strings.Contains(css, `html[data-oao-motion="reduced"] `+feedback) || !strings.Contains(css, "@media(prefers-reduced-motion:reduce) { "+feedback) {
		t.Fatal("both user and system reduced motion must disable feedback transforms")
	}
	if strings.Contains(css, "* { transform:none") {
		t.Fatal("reduced motion must preserve structural transforms such as switch state")
	}
}
