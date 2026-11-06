package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestRulePeriodicRejectsNonTestDatabase(t *testing.T) {
	for _, dsn := range []string{"", "root@tcp(127.0.0.1:1)/production", "invalid-dsn"} {
		t.Setenv("PAPA_RULE_SCHEDULE_DSN", dsn)
		var output bytes.Buffer
		if err := command(context.Background(), &output, options{}); err == nil || !strings.Contains(err.Error(), "rule_test") || output.Len() != 0 {
			t.Fatalf("DSN must be rejected before opening a connection: error=%v output=%q", err, output.String())
		}
	}
}

func TestRulePeriodicOfflineFixtures(t *testing.T) {
	registry, bindings, err := verifiedRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"catalog", "detail"} {
		ref := bindings[stage]
		if _, err := registry.Verified(ref.ID, ref.Version); err != nil {
			t.Fatalf("%s is not fixture verified: %v", stage, err)
		}
	}
}

func TestRulePeriodicBusinessSaveIsIdempotentAcrossBatches(t *testing.T) {
	store := &businessStore{records: make(map[string]businessRecord)}
	task := &engine.Task{Stage: "detail"}
	for _, batch := range []string{"first", "second"} {
		page := engine.RulePage{BatchID: batch, Result: crawlrule.Result{Records: []map[string]any{{"id": "A", "title": batch}, {"id": "B", "title": batch}}}}
		for replay := 0; replay < 2; replay++ {
			if err := store.save(context.Background(), task, page); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := []businessRecord{{ID: "A", Title: "second", BatchID: "second"}, {ID: "B", Title: "second", BatchID: "second"}}
	if got := store.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated Save appended or retained stale records: got=%+v want=%+v", got, want)
	}
}

func TestRulePeriodicExampleRealMySQLTwoBatches(t *testing.T) {
	dsn := os.Getenv("PAPA_RULE_SCHEDULE_EXAMPLE_TEST_DSN")
	if dsn == "" {
		t.Skip("set PAPA_RULE_SCHEDULE_EXAMPLE_TEST_DSN to a disposable rule_test database; real MySQL workflow was not run")
	}
	parsed, err := mysqldriver.ParseDSN(dsn)
	if err != nil || !strings.Contains(parsed.DBName, "rule_test") {
		t.Fatal("test database name must contain rule_test")
	}
	t.Setenv("PAPA_RULE_SCHEDULE_DSN", dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := command(ctx, &output, options{Migrate: true}); err != nil {
		t.Fatalf("real Engine workflow: %v\n%s", err, output.String())
	}
	var result demoResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ControlID == 0 || result.IntervalSecond != 10 || result.ControlRepeat < 2 || len(result.Batches) != 2 || !result.ReopenedEqual {
		t.Fatalf("schedule invariants: %+v", result)
	}
	seen := make(map[uint]bool)
	for i, batch := range result.Batches {
		status := batch.Status
		if status.TaskID != result.ControlID || status.Sequence != uint64(i+1) || status.Revision != uint64(i+1) || len(status.BatchID) != 64 || status.RootID == 0 || !status.Complete || status.Total != 4 || status.Success != 4 || status.Pending != 0 || status.Processing != 0 || status.Failed != 0 || status.ReleaseID != "" || status.ReleaseGeneration != 0 {
			t.Fatalf("batch %d was not independently completed: %+v", i+1, batch)
		}
		if len(batch.TaskIDs) != 4 || len(batch.Records) != 2 {
			t.Fatalf("batch %d lacks all real tasks or upserted records: %+v", i+1, batch)
		}
		foundRoot := false
		for _, id := range batch.TaskIDs {
			if id == 0 || id == result.ControlID || seen[id] {
				t.Fatalf("batch %d reused a previous task ID %d", i+1, id)
			}
			seen[id] = true
			foundRoot = foundRoot || id == status.RootID
		}
		if !foundRoot {
			t.Fatalf("batch %d is missing its fixed root", i+1)
		}
		wantRecords := []businessRecord{{ID: "A", Title: fmt.Sprintf("Alpha-v%d", i+1), BatchID: status.BatchID}, {ID: "B", Title: fmt.Sprintf("Beta-v%d", i+1), BatchID: status.BatchID}}
		if !reflect.DeepEqual(batch.Records, wantRecords) {
			t.Fatalf("batch %d business result was not updated: got=%+v want=%+v", i+1, batch.Records, wantRecords)
		}
	}
	if result.Batches[0].Status.BatchID == result.Batches[1].Status.BatchID || !reflect.DeepEqual(result.Reopened, result.Batches[1].Status) {
		t.Fatalf("new batch identity or reopened state is wrong: %+v", result)
	}
	wantRequests := map[string]int{"/list1": 2, "/list2": 2, "/detail/A": 2, "/detail/B": 2}
	if !reflect.DeepEqual(result.Requests, wantRequests) {
		t.Fatalf("real requests did not recrawl every page exactly twice: got=%v want=%v", result.Requests, wantRequests)
	}
	t.Logf("control=%d batches=%s/%s tasks=%v/%v requests=%v reopened=%v", result.ControlID, result.Batches[0].Status.BatchID, result.Batches[1].Status.BatchID, result.Batches[0].TaskIDs, result.Batches[1].TaskIDs, result.Requests, result.ReopenedEqual)
}
