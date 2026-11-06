package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

func TestRuleTaskRoleAndAdminProtection(t *testing.T) {
	for _, tc := range []struct{ meta, role string }{
		{"", "ordinary"}, {`{}`, "ordinary"}, {`null`, "ordinary"},
		{`{"_rule_batch":"x"}`, "member"}, {`{"_rule_batch":null}`, "member"},
		{`{"_rule_schedule":"x"}`, "schedule"}, {`{"_rule_schedule":false}`, "schedule"}, {`bad json`, "invalid"},
		{`{"_rule_schedule":"x","_rule_batch":"b"}`, "invalid"},
	} {
		t.Run(tc.role+tc.meta, func(t *testing.T) {
			if got := RuleTaskRole([]byte(tc.meta)); got != tc.role {
				t.Fatalf("role=%s", got)
			}
			for _, action := range []string{"delete", "repeat", "interval", "retry"} {
				f := newFakeTaskDB()
				f.row["meta"] = []byte(tc.meta)
				f.row["status"] = int64(models.TaskStatusSuccess)
				e, _ := urgentEngine(t, f)
				protected := tc.role == "invalid" || tc.role == "member" || (action == "delete" && tc.role == "schedule")
				if !protected {
					continue
				}
				var err error
				switch action {
				case "delete":
					err = e.DeleteTask(7)
				case "repeat":
					err = e.SetTaskRepeatable(7, true)
				case "interval":
					err = e.SetTaskRepeatInterval(7, 0, 60)
				case "retry":
					err = e.RetryTask(7, 0)
				}
				if !errors.Is(err, ErrTaskBatchProtected) || f.written() != "" {
					t.Fatalf("%s: err=%v SQL=%s", action, err, f.written())
				}
			}
		})
	}
}

func TestRuleScheduleDetailJSONPreservesIntegers(t *testing.T) {
	var exact uint64 = 9007199254740993
	d := RuleScheduleDetail{Batch: RuleScheduleStatus{TaskID: uint(exact), RootID: uint(exact), Revision: exact, Sequence: exact, ReleaseGeneration: exact}}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Batch map[string]any `json:"batch"`
	}
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TaskID", "RootID", "Revision", "Sequence", "ReleaseGeneration"} {
		if got.Batch[key] != "9007199254740993" {
			t.Fatalf("%s lost precision: %s", key, b)
		}
	}
	if got.Batch["Complete"] != false || got.Batch["Total"] != float64(0) {
		t.Fatalf("status fields lost: %s", b)
	}
}

func TestRuleBatchRetryGuardAndRepair(t *testing.T) {
	f := newFakeTaskDB()
	f.row["meta"] = []byte(`{"_rule_batch":"batch"}`)
	f.row["status"] = int64(models.TaskStatusFailed)
	e, _ := urgentEngine(t, f)
	if err := e.RetryTask(7, 0); err != nil {
		t.Fatal(err)
	}
	if sql := f.written(); !strings.Contains(sql, "AND (status = ? AND repeatable = ?)") {
		t.Fatalf("missing atomic guard: %s", sql)
	}
	f = newFakeTaskDB()
	f.row["meta"] = []byte(`{"_rule_batch":"batch"}`)
	f.row["repeatable"] = int64(1)
	e, _ = urgentEngine(t, f)
	if err := e.SetTaskRepeatable(7, false); err != nil {
		t.Fatalf("must allow disabling stray polling: %v", err)
	}
	db := dryDB(t)
	query := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return taskRetryScope(tx, &models.CrawlerTask{ID: 7, Meta: []byte(`{"_rule_batch":"x"}`)}, 4).Update("status", models.TaskStatusPending)
	})
	for _, want := range []string{"reprocess = 4", "status = 3", "repeatable = 0"} {
		if !strings.Contains(query, want) {
			t.Fatalf("missing %s: %s", want, query)
		}
	}
}

func TestRuleScheduleAdminListSQL(t *testing.T) {
	db := dryDB(t)
	query := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		var rows []RuleScheduleTask
		return scheduleAdminScope(tx, "example", 42).Select(scheduleTaskColumns).Limit(21).Find(&rows)
	})
	for _, want := range []string{"id > 42", "JSON_CONTAINS_PATH(meta, 'one', '$._rule_schedule') = 1", "site = 'example'", "ORDER BY id ASC", "LIMIT 21"} {
		if !strings.Contains(query, want) {
			t.Fatalf("missing %s: %s", want, query)
		}
	}
}
