package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldDoesNotRegisterBusinessPlaceholder(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatalf("runNew = %v", err)
	}
	view := readFile(t, filepath.Join("demo", "internal", "monitor", "view", "view.go"))
	for _, unwanted := range []string{`UsePage(`, `UseCSS(`, `Group: "业务"`, `Key: "demo"`, `Papa.page("demo"`} {
		if strings.Contains(view, unwanted) {
			t.Errorf("generated project pre-registers business placeholder %q", unwanted)
		}
	}
	if !strings.Contains(view, "func Register(app *papa.App)") {
		t.Fatal("business page extension point missing")
	}
}
