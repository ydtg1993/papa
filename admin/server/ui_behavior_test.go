package server

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ydtg1993/oao"
)

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
	for _, name := range []string{"visual-state.js", "visuals.js", "appearance.js", "extensions.js", "navigation.js", "requests.js", "table-links.js", "schedules.js", "mo.js", "trace.js", "workbench.js", "rules.js", "quality.js", "quality-write.js", "agents.js"} {
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
	for _, script := range []string{"appearance_test.js", "storage_startup_test.js", "visual_state_test.js", "visuals_test.js", "navigation_test.js", "monitor_identity_test.js", "legacy_oao_test.js", "extensions_test.js", "monitor_requests_test.js", "native_session_test.js", "table_links_test.js", "schedules_test.js", "dialog_test.js", "workbench_test.js", "rules_test.js", "quality_test.js", "quality_write_test.js", "agents_test.js"} {
		t.Run(script, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if output, err := exec.CommandContext(ctx, node, filepath.Join("testdata", script), dir).CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
		})
	}
}
