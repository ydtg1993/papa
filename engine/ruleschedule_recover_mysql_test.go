package engine

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This wrapper creates the actual production state/claim through Engine1, then
// reproduces a process stopping after the batch root INSERT and before enqueue.
type schedulePersistBeforeEnqueue struct {
	schedule *RuleSchedule
	stored   chan uint
}

func (f *schedulePersistBeforeEnqueue) GetStage() string { return f.schedule.GetStage() }
func (f *schedulePersistBeforeEnqueue) FetchHandler(ctx context.Context, task *Task, e *Engine) error {
	return f.schedule.runSchedule(ctx, task, e, func(root *Task) error {
		if err := root.Insert(e.db); err != nil {
			return err
		}
		select {
		case f.stored <- uint(root.ID):
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	})
}

func TestRuleScheduleMySQLRecoverUnqueuedRootAcrossEngines(t *testing.T) {
	dsn := os.Getenv("PAPA_RULE_SCHEDULE_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_RULE_SCHEDULE_TEST_DSN not set; real Engine restart verification not run")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_rule_test_schedule" {
		t.Fatal("requires dedicated papa_rule_test_schedule database")
	}
	open := func() (*gorm.DB, *sql.DB) {
		db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		return db, raw
	}
	db1, raw1 := open()
	if err = db1.AutoMigrate(&models.CrawlerTask{}, &models.RuleSchedule{}); err != nil {
		raw1.Close()
		t.Fatal(err)
	}
	site := "restart-" + uuid.NewString()
	var controllerID uint
	cleanupDB := db1
	connections := []*sql.DB{raw1}
	var engines []*Engine
	t.Cleanup(func() {
		for i := len(engines) - 1; i >= 0; i-- {
			engines[i].Stop(5 * time.Second)
		}
		cleanupDB.Where("task_id = ?", controllerID).Delete(&models.RuleSchedule{})
		cleanupDB.Where("site = ?", site).Delete(&models.CrawlerTask{})
		for _, connection := range connections {
			connection.Close()
		}
	})
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/list":
			fmt.Fprint(w, `{"rows":[{"id":"1","url":"/detail1"}],"next":"/page2"}`)
		case "/page2":
			fmt.Fprint(w, `{"rows":[{"id":"2","url":"/detail2"}]}`)
		case "/detail1":
			fmt.Fprint(w, `{"rows":[{"id":"1"}]}`)
		case "/detail2":
			fmt.Fprint(w, `{"rows":[{"id":"2"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	registry := ruleTestRegistry(t, srv.URL)
	catalog := &testRuleCatalog{registry: registry}
	catalog.publish("v1", 1)
	catalog.snapshot.Site = site
	publicationReads := func() int { catalog.mu.Lock(); defer catalog.mu.Unlock(); return catalog.snapshots }
	var savedMu sync.Mutex
	saved := map[string][]string{}
	newFetchers := func() (*RuleSchedule, *RuleFetcher, *RuleFetcher) {
		cfg := ruleTestConfig(registry, srv.URL)
		cfg.Site, cfg.Registry, cfg.Bindings, cfg.Catalog = site, nil, nil, catalog
		cfg.Save = func(_ context.Context, task *Task, page RulePage) error {
			savedMu.Lock()
			defer savedMu.Unlock()
			saved[page.BatchID] = append(saved[page.BatchID], task.Stage+":"+page.Rule.Version)
			return nil
		}
		cfg.Stage = "detail"
		detail := ruleTestFetcher(t, cfg)
		cfg.Stage = "list"
		cfg.Links = []RuleLink{{Field: "url", Stage: "detail"}}
		entry := ruleTestFetcher(t, cfg)
		schedule, err := NewRuleSchedule(RuleScheduleConfig{Stage: "schedule", Entry: entry, Interval: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return schedule, entry, detail
	}
	engineConfig := func(recover bool) *config.Config {
		return &config.Config{
			HTML:         config.HTMLConfig{Enable: true, Timeout: 2 * time.Second, MaxBodySize: 64 << 10},
			Crawler:      config.CrawlerConfig{DrainInterval: 10 * time.Millisecond, DedupCacheSize: 1024},
			RecoverQueue: config.RecoverQueueConfig{Enabled: recover, WorkerCount: 1, BatchSize: 16},
			RepeatQueue:  config.RepeatQueueConfig{Enabled: recover, WorkerCount: 1, BatchSize: 16, Interval: time.Hour},
		}
	}
	register := func(e *Engine, schedule Fetcher, entry, detail *RuleFetcher) {
		e.SetSite(core.Site{Key: site, BaseURL: srv.URL})
		e.SetHTMLClient()
		for _, f := range []Fetcher{schedule, entry, detail} {
			e.AddStage(f.GetStage(), StageConfig{Site: site, WorkerCount: 1, QueueSize: 16, MaxAttempts: 1}, f, nil)
		}
		e.ApplyRegisterStage()
	}
	s1, entry1, detail1 := newFetchers()
	e1 := NewEngine(db1, engineConfig(false), testLoggerSet())
	engines = append(engines, e1)
	persistOnly := &schedulePersistBeforeEnqueue{schedule: s1, stored: make(chan uint, 1)}
	register(e1, persistOnly, entry1, detail1)
	control, err := s1.NewTask(RuleRequest{URL: srv.URL + "/list"}, map[string]string{"job": "restart"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e1.SubmitTask(control); err != nil {
		t.Fatal(err)
	}
	controllerID = uint(control.ID)
	var originalRoot uint
	select {
	case originalRoot = <-persistOnly.stored:
	case <-time.After(10 * time.Second):
		t.Fatal("Engine1 did not persist root")
	}
	if drained, _ := e1.Stop(5 * time.Second); !drained {
		t.Fatal("Engine1 did not stop")
	}
	first, err := s1.Status(context.Background(), e1, controllerID)
	if err != nil || first.Sequence != 1 || first.RootID != originalRoot || first.Pending != 1 || first.Complete || publicationReads() != 1 || requests.Load() != 0 {
		t.Fatalf("before restart: %+v reads=%d requests=%d err=%v", first, publicationReads(), requests.Load(), err)
	}
	// Prevent the poller from issuing a new tick during startup recovery. The
	// subsequent explicit ForceRepoll below exercises the real repeat path.
	if err = db1.Model(&models.CrawlerTask{}).Where("id = ?", controllerID).Update("next_repeat_at", time.Now().Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err = raw1.Close(); err != nil {
		t.Fatal(err)
	}
	catalog.publish("v2", 2)
	catalog.snapshot.Site = site
	db2, raw2 := open()
	cleanupDB = db2
	connections = append(connections, raw2)
	s2, entry2, detail2 := newFetchers()
	e2 := NewEngine(db2, engineConfig(true), testLoggerSet())
	engines = append(engines, e2)
	register(e2, s2, entry2, detail2) // starts the actual one-time recover_queue.
	waitComplete := func(sequence uint64) RuleScheduleStatus {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			status, err := s2.Status(context.Background(), e2, controllerID)
			if err != nil {
				t.Fatal(err)
			}
			var controller models.CrawlerTask
			if err = db2.Where("id = ?", controllerID).Take(&controller).Error; err != nil {
				t.Fatal(err)
			}
			if status.Sequence > sequence || status.Failed > 0 || controller.Status == models.TaskStatusFailed {
				t.Fatalf("unexpected batch/controller: %+v %+v", status, controller)
			}
			if status.Sequence == sequence && status.Complete && controller.Status == models.TaskStatusSuccess {
				return status
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("batch %d did not complete", sequence)
		return RuleScheduleStatus{}
	}
	recovered := waitComplete(1)
	if recovered.RootID != originalRoot || recovered.BatchID != first.BatchID || recovered.ReleaseID != "release-1" || recovered.Total != 4 || publicationReads() != 1 || requests.Load() != 4 || e2.recoveredCount.Load() < 2 {
		t.Fatalf("recovery rebound or failed: %+v reads=%d requests=%d recovered=%d", recovered, publicationReads(), requests.Load(), e2.recoveredCount.Load())
	}
	assertSaved := func(batch, version string) {
		savedMu.Lock()
		defer savedMu.Unlock()
		values := saved[batch]
		if len(values) != 4 {
			t.Fatalf("batch %s has %d saves", batch, len(values))
		}
		counts := map[string]int{}
		for _, value := range values {
			counts[value]++
		}
		if counts["list:"+version] != 2 || counts["detail:"+version] != 2 {
			t.Fatalf("wrong versions: %v", values)
		}
	}
	assertSaved(first.BatchID, "v1")
	if count, err := e2.ForceRepollRepeatableTasks(); err != nil || count != 1 {
		t.Fatalf("subsequent tick: count=%d err=%v", count, err)
	}
	second := waitComplete(2)
	if second.BatchID == first.BatchID || second.RootID == originalRoot || second.ReleaseID != "release-2" || publicationReads() != 2 || requests.Load() != 8 {
		t.Fatalf("new tick did not adopt new publication: %+v reads=%d requests=%d", second, publicationReads(), requests.Load())
	}
	assertSaved(second.BatchID, "v2")
	if err = e2.SetTaskRepeatable(controllerID, false); err != nil {
		t.Fatal(err)
	}
	if drained, _ := e2.Stop(5 * time.Second); !drained {
		t.Fatal("Engine2 did not stop")
	}
}
