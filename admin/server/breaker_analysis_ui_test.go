package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBreakerAnalysisDialogBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js required")
	}
	raw, err := staticFS.ReadFile("static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "mo.js")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, node, "testdata/breaker_analysis_test.js", source).CombinedOutput(); err != nil {
		t.Fatalf("dialog behavior: %v\n%s", err, out)
	}
}
