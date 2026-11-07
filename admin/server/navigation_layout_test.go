package server

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

func TestMonitorLayoutHasFourCategoriesAndSeparateLogs(t *testing.T) {
	b, err := templateFS.ReadFile("template.html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Find("#main-nav button[data-section]").Length(); got != 4 {
		t.Fatalf("primary navigation must have four categories, got %d", got)
	}
	for _, section := range []string{"flow", "monitor", "data", "system"} {
		if doc.Find(`#main-nav button[data-section="`+section+`"]`).Length() != 1 {
			t.Errorf("missing/duplicate section %s", section)
		}
	}
	if doc.Find("#main-nav [data-module], #main-nav [data-page], #main-nav [data-oao]").Length() != 0 {
		t.Fatal("primary navigation still contains individual feature pages")
	}
	if doc.Find("#section-nav").Length() != 1 || doc.Find("#section-empty").Length() != 1 {
		t.Fatal("category navigation or empty state missing")
	}
	if doc.Find("#mod-logs #log-list").Length() != 1 || doc.Find("#mod-settings #log-list").Length() != 0 {
		t.Fatal("runtime logs must belong to their own monitor page")
	}
	for _, path := range []string{"navigation.js", "layout.css"} {
		if _, err := staticFS.ReadFile("static/" + path); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "/static/"+path) {
			t.Fatalf("missing embedded resource reference: %s", path)
		}
	}
}

func TestMonitorOverviewHasOneDefaultChart(t *testing.T) {
	b, err := templateFS.ReadFile("template.html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"visual-queue-trend", "visual-stage-bars", "visual-results", "visual-cpu", "visual-mem", "visual-disk"} {
		node := doc.Find("#" + id)
		if node.Length() != 1 {
			t.Fatalf("missing/duplicate chart host %s", id)
		}
		details := node.ParentsFiltered("details:not([open])")
		if id == "visual-queue-trend" && details.Length() != 0 {
			t.Fatal("main trend must remain immediately visible")
		}
		if id != "visual-queue-trend" && details.Length() == 0 {
			t.Errorf("detail chart %s is expanded by default", id)
		}
	}
	for _, id := range []string{"workbench-analysis", "workbench-resources", "workbench-stage-details"} {
		if doc.Find("details#"+id+":not([open])").Length() != 1 {
			t.Errorf("detail section must start collapsed: %s", id)
		}
	}
}
