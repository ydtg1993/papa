package server

import (
	"io/fs"
	"strings"
	"testing"
)

func TestWorkbenchTemplateKeepsOperationsAndDetails(t *testing.T) {
	b, err := fs.ReadFile(templateFS, "template.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, id := range []string{"workbench-kpis", "workbench-issues", "workbench-sites", "workbench-freshness", "workbench-stage-details", "workbench-resources", "dash-queue", "dash-sys", "dash-dirs", "dash-site-ctl"} {
		if strings.Count(html, `id="`+id+`"`) != 1 {
			t.Errorf("missing/duplicate DOM target %s", id)
		}
	}
	if strings.Index(html, `id="workbench-issues"`) > strings.Index(html, `id="dash-sys"`) {
		t.Fatal("resources displaced operations")
	}
	if strings.Index(html, `/static/workbench.js`) > strings.Index(html, `/static/mo.js`) {
		t.Fatal("workbench must load before initial monitor polling")
	}
	if strings.Index(html, `/static/workbench.css`) > strings.Index(html, `/static/custom.css`) {
		t.Fatal("business CSS must retain override priority")
	}
	for _, name := range []string{"static/workbench.js", "static/workbench.css"} {
		if _, err = staticFS.ReadFile(name); err != nil {
			t.Fatal(err)
		}
	}
	js, err := staticFS.ReadFile("static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"renderWorkbench(data);", "setWorkbenchConnection(ok, text);"} {
		if !strings.Contains(string(js), call) {
			t.Fatalf("workbench not connected: %s", call)
		}
	}
}
