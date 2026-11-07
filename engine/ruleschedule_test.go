package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

func scheduleTest(t *testing.T) (*RuleSchedule, *testRuleCatalog) {
	t.Helper()
	reg := ruleTestRegistry(t, "https://example.com")
	catalog := &testRuleCatalog{registry: reg}
	catalog.publish("v1", 1)
	cfg := ruleTestConfig(reg, "https://example.com")
	cfg.Registry = nil
	cfg.Bindings = nil
	cfg.Catalog = catalog
	s, err := NewRuleSchedule(RuleScheduleConfig{Stage: "schedule", Entry: ruleTestFetcher(t, cfg), Interval: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s, catalog
}

func TestRuleSchedulePureIdentityAndBounds(t *testing.T) {
	s, catalog := scheduleTest(t)
	request := RuleRequest{URL: "https://example.com/list"}
	first, err := s.NewTask(request, map[string]string{"job": "A"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.NewTask(request, map[string]string{"job": "A"})
	if first.URL != again.URL || !first.Repeatable || catalog.snapshots != 0 {
		t.Fatal("constructor consumed publication or unstable control identity")
	}
	if _, err = s.NewTask(request, map[string]string{"_rule_batch": strings.Repeat("a", 64)}); err == nil {
		t.Fatal("reserved batch accepted")
	}
	if _, err = s.NewTask(request, map[string]string{"payload": strings.Repeat("\"", 600000)}); err == nil {
		t.Fatal("outer Meta exceeds storage budget")
	}
	if _, err = s.NewTask(RuleRequest{URL: "https://other.example/list"}, nil); err == nil {
		t.Fatal("out-of-scope entry accepted")
	}
	for _, d := range []time.Duration{0, 9 * time.Second, 10500 * time.Millisecond} {
		if _, err = NewRuleSchedule(RuleScheduleConfig{Stage: "schedule", Entry: s.config.Entry, Interval: d}); err == nil {
			t.Fatalf("invalid interval %s", d)
		}
	}
	row := first.toModel()
	row.ID = 1
	row.Status = models.TaskStatusProcessing
	c, err := s.decodeControl(row)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.prepareBatch(context.Background(), c, 1)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.snapshots != 1 || !validRuleBatch(p.BatchID) {
		t.Fatal("batch not pinned exactly once")
	}
	b, err := s.encodePayload(c, p)
	if err != nil {
		t.Fatal(err)
	}
	state := models.RuleSchedule{TaskID: 1, Revision: 1, PayloadJSON: string(b), PayloadSHA256: ruleDigest(b)}
	// Changing startup defaults cannot rebind an already fixed checkpoint.
	s.config.Entry.config.MaxPages = 20
	if _, err = s.decodeState(c, state); err != nil {
		t.Fatalf("fixed checkpoint rejected after config change: %v", err)
	}
	state.PayloadJSON += " "
	if _, err = s.decodeState(c, state); !errors.Is(err, ErrRuleScheduleCorrupt) {
		t.Fatal("digest corruption accepted")
	}
	p.Root.Meta[ruleBatchMeta] = strings.Repeat("A", 64)
	if _, err = s.encodePayload(c, p); err == nil {
		t.Fatal("invalid batch snapshot accepted")
	}
}

func TestRuleScheduleSQLShapes(t *testing.T) {
	db := dryRunMySQL(t)
	sql := renderSQL(t, db, func(tx *gorm.DB) *gorm.DB { return scheduleStateScope(tx, 7, 3).Updates(map[string]any{"revision": 4}) })
	if !strings.Contains(sql, "task_id = ? AND revision = ?") {
		t.Fatalf("missing state CAS: %s", sql)
	}
	sql = renderSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return scheduleBatchScope(tx, "site", strings.Repeat("a", 64), 10).Select("status, COUNT(*) AS count").Group("status").Find(&[]models.CrawlerTask{})
	})
	for _, part := range []string{"site = ? AND id >= ?", "JSON_UNQUOTE(JSON_EXTRACT(meta, '$._rule_batch')) = ?", "GROUP BY `status`"} {
		if !strings.Contains(sql, part) {
			t.Fatalf("missing %s: %s", part, sql)
		}
	}
}
