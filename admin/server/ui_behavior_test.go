package server

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/oao"
)

// Every local asset referenced by the shipped page must be embedded. Custom
// assets and optional oao bundles are served by their own handlers.
func TestMonitorTemplateLocalAssetsExist(t *testing.T) {
	b, err := templateFS.ReadFile("template.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)="/static/([^"]+)"`).FindAllSubmatch(b, -1)
	if len(refs) == 0 {
		t.Fatal("monitor has no asset references")
	}
	for _, ref := range refs {
		name := string(ref[1])
		if name == "custom.js" || name == "custom.css" || strings.HasPrefix(name, "oao/") {
			continue
		}
		if _, err := staticFS.ReadFile("static/" + name); err != nil {
			t.Errorf("template references unavailable asset %s: %v", name, err)
		}
	}
}

func TestOperationsJavaScriptBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for UI behavior tests")
	}
	dir := t.TempDir()
	template, err := templateFS.ReadFile("template.html")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "template.html"), template, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"queue-governance.js", "visual-state.js", "visuals.js", "appearance.js", "extensions.js", "navigation.js", "requests.js", "table-links.js", "mo.js", "trace.js", "workbench.js"} {
		b, err := staticFS.ReadFile("static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	assets, err := new(oao.Oao).StaticFS()
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(assets, "oao.js")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "oao.js"), b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"controller_lifecycle_test.js", "breaker_control_test.js", "queue_governance_test.js", "trace_dialog_test.js", "appearance_test.js", "storage_startup_test.js", "visual_state_test.js", "visuals_test.js", "navigation_test.js", "monitor_identity_test.js", "legacy_oao_test.js", "extensions_test.js", "monitor_requests_test.js", "table_links_test.js", "dialog_test.js", "workbench_test.js"} {
		t.Run(script, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(ctx, node, filepath.Join("testdata", script), dir).CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
		})
	}
}
