package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
)

func TestSmartBreakerMySQLRestartAndManualClosure(t *testing.T) {
	db, _ := compensationMySQL(t)
	if err := db.AutoMigrate(&models.BreakerIncident{}, &models.BreakerHistory{}); err != nil {
		t.Fatal(err)
	}
	makeRuntime := func() (*Engine, func()) {
		e := &Engine{db: db}
		e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1})
		ctx, cancel := context.WithCancel(context.Background())
		if err := e.InitBreakerRuntime(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		return e, func() { cancel(); e.tickerWG.Wait() }
	}
	first, stopFirst := makeRuntime()
	smartFailure(first, 1, "detail", "missing_title")
	flushSmart(t, first)
	want := smartLifecycle(first)
	stopFirst()
	second, stopSecond := makeRuntime()
	defer stopSecond()
	got := smartLifecycle(second)
	if got.Capture.ID != want.Capture.ID || got.State != "cooldown" || !second.isPaused(pauseScope{Site: "synthetic"}) {
		t.Fatalf("restart lost persisted protection: %+v", got)
	}
	if !got.Deadline.Equal(want.Deadline) {
		t.Fatalf("restart extended recovery budget: %v -> %v", want.Deadline, got.Deadline)
	}
	if second.breakerSum("synthetic") != 1 {
		t.Fatal("MySQL restart erased failure window")
	}
	if !second.PauseSite("synthetic", "manual takeover expires with process") {
		t.Fatal("manual takeover did not close event")
	}
	flushSmart(t, second)
	third, stopThird := makeRuntime()
	defer stopThird()
	if third.isPaused(pauseScope{Site: "synthetic"}) {
		t.Fatal("closed event restored a pause")
	}
	if third.breakerSum("synthetic") != 1 {
		t.Fatal("manual closure erased failure window on restart")
	}
	reports, err := third.BreakerReports(context.Background(), "synthetic")
	if err != nil || len(reports) != 1 || reports[0].State != "closed" || reports[0].FinalAt.IsZero() {
		t.Fatalf("final history: %+v %v", reports, err)
	}
	all, err := third.BreakerReports(context.Background(), "")
	if err != nil || len(all) != 1 {
		t.Fatalf("all-site durable history: %+v %v", all, err)
	}
}

func TestSmartBreakerStopFlushesFinalState(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	r := &memoryBreakerRepository{}
	e.breakers.breakerStore, e.breakers.breakerRuntime = r, true
	e.SetSiteBreaker("synthetic", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1})
	smartFailure(e, 1, "detail", "missing")
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("clean breaker flush reported unfinished")
	}
	rows, err := r.LoadActive(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("shutdown lost last event: %+v %v", rows, err)
	}
	if !strings.Contains(rows[0].Text(), "process_stopped") {
		t.Fatal("normal stop omitted from incident history")
	}
}
