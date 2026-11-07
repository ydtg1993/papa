package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldCustomPageDefaultsToDataSection(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatalf("runNew = %v", err)
	}
	view := readFile(t, filepath.Join("demo", "internal", "monitor", "view", "view.go"))
	for _, want := range []string{`Section: "data"`, `Group: "业务"`, `Key: "demo"`, `Papa.page("demo"`} {
		if !strings.Contains(view, want) {
			t.Errorf("generated custom page missing %q", want)
		}
	}
}
