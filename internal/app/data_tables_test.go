package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/oao"
)

type dataTaskActions struct {
	retried uint
	version int
	failed  uint
	reason  string
}

func (a *dataTaskActions) RetryTaskVersioned(_ context.Context, id uint, version int, _ uint64) error {
	a.retried = id
	a.version = version
	return nil
}
func (a *dataTaskActions) MarkTaskFailed(id uint, reason string) error {
	a.failed = id
	a.reason = reason
	return nil
}
func (*dataTaskActions) DeleteTask(uint) error                      { return nil }
func (*dataTaskActions) UrgentTask(uint) error                      { return nil }
func (*dataTaskActions) SetTaskRepeatable(uint, bool) error         { return nil }
func (*dataTaskActions) SetTaskRepeatInterval(uint, int, int) error { return nil }

func TestTaskDataTablesOnlyTaskWithTraceAction(t *testing.T) {
	a := &dataTaskActions{}
	tables := taskDataTables(nil, a)
	if len(tables) != 1 || tables[0].Key != "task" {
		t.Fatalf("unexpected data tables: %+v", tables)
	}
	if !strings.Contains(tables[0].Label, "crawler_tasks") || len(tables[0].Actions) != 8 {
		t.Fatal("task operations or read-only trace boundary changed")
	}
	o, err := oao.New(oao.Config{Tables: tables})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	o.Mount(mux)
	post := func(action, body string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/oao/task/action/"+action, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := post("retry", `{"id":"7","row":{"reprocess":3,"compensation_version":0}}`); code != 200 || a.retried != 7 || a.version != 3 {
		t.Fatalf("retry status=%d id=%d version=%d", code, a.retried, a.version)
	}
	a.retried = 0
	if code := post("retry", `{"id":"7","row":{}}`); code != 400 || a.retried != 0 {
		t.Fatal("missing version accepted", code)
	}
	if code := post("fail", `{"id":"8","values":{"reason":"人工确认失败"}}`); code != 200 || a.failed != 8 || a.reason != "人工确认失败" {
		t.Fatal("mark failed not wired", code)
	}
	if code := post("trace", `{"id":"7"}`); code != 200 || a.retried != 0 {
		t.Fatal("trace action failed or mutated task", code)
	}
	for _, key := range []string{"crawler_task_trace", "crawler_config", "crawler_access_token", "crawler_rule_versions", "crawler_agent_runs", "site", "crawler_tasks"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/oao/"+key, nil))
		if w.Code != 404 {
			t.Fatal("unexpected duplicate/system table", key, w.Code)
		}
	}
}
