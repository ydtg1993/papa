package engine

import (
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"strings"
	"testing"
)

func TestTaskTraceExecutionSchema(t *testing.T) {
	db := dryDB(t)
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&models.TaskTrace{}); err != nil {
		t.Fatal(err)
	}
	field := stmt.Schema.LookUpField("execution_id")
	if field == nil {
		t.Fatal("execution identity column missing")
	}
	if !field.NotNull || !field.HasDefaultValue || field.DefaultValue != "" {
		t.Fatalf("legacy rows require NOT NULL DEFAULT empty: %+v", field)
	}
	if got := db.Migrator().FullDataTypeOf(field).SQL; !strings.HasPrefix(got, "varchar(36) NOT NULL DEFAULT ''") {
		t.Fatalf("execution identity DDL: %s", got)
	}
	found := false
	for _, idx := range stmt.Schema.ParseIndexes() {
		if idx.Name != "idx_trace_execution" {
			continue
		}
		found = true
		if len(idx.Fields) != 2 || idx.Fields[0].DBName != "task_id" || idx.Fields[1].DBName != "execution_id" {
			t.Fatalf("execution lookup needs ordered (task_id, execution_id) index: %+v", idx)
		}
		if idx.Class == "UNIQUE" {
			t.Fatal("one execution must allow multiple steps")
		}
	}
	if !found {
		t.Fatal("execution lookup index missing")
	}
}

func TestFakeTraceColumnsCoverModel(t *testing.T) {
	stmt := &gorm.Statement{DB: dryDB(t)}
	if err := stmt.Parse(&models.TaskTrace{}); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(fakeTraceColumns))
	for _, name := range fakeTraceColumns {
		if got[name] {
			t.Errorf("duplicate fake trace column %q", name)
		}
		got[name] = true
	}
	for _, name := range stmt.Schema.DBNames {
		if !got[name] {
			t.Errorf("fakeTraceColumns silently drops model column %q", name)
		}
	}
	if len(got) != len(stmt.Schema.DBNames) {
		t.Errorf("fake trace has %d columns, model has %d", len(got), len(stmt.Schema.DBNames))
	}
}

func TestListTraceClaimSelectedBeforeNewRoundFlush(t *testing.T) {
	f := newFakeTaskDB()
	old := traceRow(1, 7, 0, 0, "old failure", int64(models.TraceFailed), nil)
	old["execution_id"] = "old"
	f.traceRows = []fakeRow{old}
	f.onQuery = func(n int) {
		if n != 2 {
			return
		}
		fresh := traceRow(2, 7, 0, 0, "new success", int64(models.TraceOK), nil)
		fresh["execution_id"] = "new"
		f.traceRows = append(f.traceRows, fresh)
	}
	steps, err := traceDBEngine(t, f).ListTraceExecution(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].ExecutionID != "old" || steps[0].Step != "old failure" {
		t.Fatalf("two queries mixed execution identities: %+v", steps)
	}
}

func TestTaskTraceOrderedReadIndexes(t *testing.T) {
	stmt := &gorm.Statement{DB: dryDB(t)}
	if err := stmt.Parse(&models.TaskTrace{}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"idx_trace_latest": "task_id,id", "idx_trace_tail": "task_id,execution_id,attempt,seq,id"}
	for _, idx := range stmt.Schema.ParseIndexes() {
		expected, ok := want[idx.Name]
		if !ok {
			continue
		}
		var columns []string
		for _, field := range idx.Fields {
			columns = append(columns, field.DBName)
		}
		if strings.Join(columns, ",") != expected {
			t.Fatalf("%s = %v", idx.Name, columns)
		}
		delete(want, idx.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing ordered trace indexes: %v", want)
	}
}
