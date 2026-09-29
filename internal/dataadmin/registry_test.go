package dataadmin

import (
	"testing"

	"github.com/ydtg1993/papa/v2/models"
)

func TestRegisterIntrospectsColumns(t *testing.T) {
	r := New(nil)
	if err := r.Register("task", "任务", &models.CrawlerTask{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	info, ok := r.Get("task")
	if !ok {
		t.Fatal("expected task model registered")
	}
	if info.Table == "" {
		t.Fatal("expected non-empty table name")
	}

	found := map[string]bool{}
	for _, c := range info.Columns {
		found[c.Name] = true
	}
	for _, name := range []string{"id", "stage", "url", "title", "status", "error", "updated_at"} {
		if !found[name] {
			t.Errorf("expected column %q in %v", name, info.Columns)
		}
	}

	for _, c := range info.Columns {
		switch c.Name {
		case "status":
			if !c.Filterable || !c.Sortable || c.Kind != KindNumber {
				t.Errorf("status: want filterable/sortable number, got %+v", c)
			}
		case "stage":
			if !c.Searchable || c.Kind != KindString {
				t.Errorf("stage: want searchable string, got %+v", c)
			}
		case "content":
			if c.Kind != KindJSON {
				t.Errorf("content: want json kind, got %v", c.Kind)
			}
		}
	}
}
