package executionpolicy

import (
	"os/exec"
	"testing"
)

func TestEditorConflictAndUncertainOperationLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	out, err := exec.Command(node, "ui_test.js", "static/controller.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
