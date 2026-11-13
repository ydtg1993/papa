package engine

import (
	"github.com/ydtg1993/papa/v4/models"
	"testing"
)

func TestTaskIdentityMySQLDistinctIDsSameURLAndRestart(t *testing.T) {
	db, makeEngine := compensationMySQL(t)
	e := makeEngine()
	a := &Task{Site: "synthetic", Stage: "detail", URL: "https://example.invalid/resource", ResourceID: "a"}
	b := &Task{Site: a.Site, Stage: a.Stage, URL: a.URL, ResourceID: "b"}
	if err := e.SubmitTasks([]*Task{a, b}); err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatal("different IDs sharing URL merged")
	}
	e2 := makeEngine()
	c := &Task{Site: a.Site, Stage: a.Stage, URL: "https://example.invalid/moved", ResourceID: "a"}
	if err := e2.SubmitTask(c); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&models.CrawlerTask{}).Count(&count)
	if count != 2 {
		t.Fatalf("restart dedup rows=%d", count)
	}
	var stored models.CrawlerTask
	if err := db.First(&stored, a.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.URL != a.URL || stored.IdentityType != "id" || stored.IdentityValue != "a" {
		t.Fatal("identity or URL overwritten", stored)
	}
	// Repeatable and batch hits must also execute the stored URL, never a URL
	// supplied by a later duplicate submission using the same resource ID.
	for _, batch := range []bool{false, true} {
		duplicate := &Task{Site: a.Site, Stage: a.Stage, URL: "https://example.invalid/replacement", ResourceID: "a", Repeatable: true}
		var err error
		if batch {
			err = e2.SubmitTasks([]*Task{duplicate})
		} else {
			err = e2.SubmitTask(duplicate)
		}
		if err != nil {
			t.Fatal(err)
		}
		if duplicate.URL != a.URL || duplicate.ID != a.ID {
			t.Fatal("duplicate execution URL changed", batch, duplicate)
		}
	}
}
