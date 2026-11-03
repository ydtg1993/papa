package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestObsReportLatestRequestWins(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the report loader behavior test")
	}
	script, err := staticFS.ReadFile("static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "mo.js")
	if err := os.WriteFile(source, script, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "testdata/obs_report_race_test.js", source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("report loader regression: %v\n%s", err, output)
	}
}
