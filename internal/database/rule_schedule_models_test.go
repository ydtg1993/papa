package database

import (
	"reflect"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v4/config"
	"gorm.io/gorm/schema"
)

func TestRuleScheduleFrameworkModelIsOptIn(t *testing.T) {
	cfg := &config.Config{}
	for _, model := range FrameworkModels(cfg) {
		if model.Name == "crawler_rule_schedules" {
			t.Fatal("rules disabled must not require the schedule table")
		}
	}
	cfg.Rules.Enabled = true
	var schedule any
	for _, model := range FrameworkModels(cfg) {
		if model.Name == "crawler_rule_schedules" {
			if schedule != nil {
				t.Fatal("schedule table registered more than once")
			}
			schedule = model.Value
		}
	}
	if schedule == nil {
		t.Fatal("rules enabled must register crawler_rule_schedules")
	}
	parsed, err := schema.Parse(schedule, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Table != "crawler_rule_schedules" {
		t.Fatalf("schedule table = %q", parsed.Table)
	}
	if len(parsed.DBNames) != 4 || len(parsed.PrimaryFields) != 1 {
		t.Fatalf("schedule must have exactly four columns and one primary key: %+v", parsed.DBNames)
	}
	id := parsed.LookUpField("task_id")
	if id == nil || !id.PrimaryKey || id.AutoIncrement || id.FieldType.Kind() != reflect.Uint {
		t.Fatalf("task_id must be the supplied uint control-task primary key: %+v", id)
	}
	revision := parsed.LookUpField("revision")
	if revision == nil || revision.FieldType.Kind() != reflect.Uint64 || !revision.NotNull {
		t.Fatalf("revision must be a non-null uint64: %+v", revision)
	}
	for name, dataType := range map[string]string{"payload_sha256": "char(64)", "payload_json": "longtext"} {
		field := parsed.LookUpField(name)
		if field == nil || string(field.DataType) != dataType || !field.NotNull {
			t.Fatalf("%s must be non-null %s: %+v", name, dataType, field)
		}
	}
	for _, field := range parsed.Fields {
		if field.Comment == "" {
			t.Errorf("schedule column %s must have a comment", field.DBName)
		}
	}
}
