package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/admin/oplog"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/database"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// Synthetic business fixtures belong to this acceptance test, not migrations.
type appMixEffect struct {
	TaskID           uint `gorm:"primaryKey;autoIncrement:false"`
	Site, Stage, URL string
}
type appMixAttempt struct {
	ID        uint
	TaskID    uint
	StartedAt time.Time `gorm:"precision:6"`
}
type appMixControl struct {
	ID       uint
	Token    string
	Ready    bool
	Total    int
	Retained bool
}
type appMixFetcher struct {
	stage string
	run   func(context.Context, *engine.Task, *engine.Engine) error
}

func (f appMixFetcher) GetStage() string { return f.stage }
func (f appMixFetcher) FetchHandler(c context.Context, t *engine.Task, e *engine.Engine) error {
	return f.run(c, t, e)
}

func appMixArtifacts(t *testing.T, dir, outcome string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, "logs", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := 0, 0
	runID := ""
	scan := bufio.NewScanner(bytes.NewReader(content))
	scan.Buffer(make([]byte, 4096), 1024*1024)
	for scan.Scan() {
		var row struct {
			Type, Outcome string
			RunID         string `json:"run_id"`
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		if row.Type == "run_started" {
			started++
			runID = row.RunID
			if stopped != 0 || runID == "" {
				t.Fatal("invalid run start ordering/identity")
			}
		}
		if row.Type == "run_stopped" {
			stopped++
			if started != 1 || row.RunID != runID {
				t.Fatal("stop does not match started run")
			}
			if row.Outcome != outcome {
				t.Fatalf("run outcome %s want %s", row.Outcome, outcome)
			}
		}
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	if started != 1 || stopped != 1 {
		t.Fatalf("lifecycle events: %d/%d", started, stopped)
	}
	days, err := os.ReadDir(filepath.Join(dir, "reports"))
	if err != nil || len(days) == 0 {
		t.Fatal("report missing", err)
	}
	reportDays := 0
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		reportDays++
		for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
			data, err := os.ReadFile(filepath.Join(dir, "reports", day.Name(), name))
			if err != nil || len(data) == 0 {
				t.Fatal("report artifact missing", name, err)
			}
			if strings.HasSuffix(name, ".json") && !json.Valid(data) {
				t.Fatal("invalid report JSON")
			}
		}
	}
	if reportDays == 0 {
		t.Fatal("no report day artifacts")
	}
}

func appMixPoolClosed(t *testing.T, a *App, closed bool) {
	t.Helper()
	if a.BrowserPool == nil {
		t.Fatal("browser pool was not assembled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// No direct slots: this probes admission without launching Chromium.
	_, err := a.BrowserPool.GetDirect(ctx)
	want := "direct browsers not configured"
	if closed {
		want = "browser pool closed"
	}
	if err == nil || err.Error() != want {
		t.Fatalf("browser pool closed=%v: %v", closed, err)
	}
}

func appMixStopped(t *testing.T, a *App, client *http.Client, health string) {
	t.Helper()
	if a.runtimeState.done == nil || a.messageListeners.done == nil || a.reportRunner == nil || a.oplog == nil {
		t.Fatal("Run did not assemble expected lifecycle components")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !waitAppLoop(ctx, a.runtimeState.done) || !waitAppLoop(ctx, a.messageListeners.done) {
		t.Fatal("owned App loops not stopped")
	}
	if res, err := client.Get(health); err == nil {
		res.Body.Close()
		t.Fatal("HTTP listener remained open after Run")
	}
	// A request timeout is not evidence that the listening socket closed.
	address := strings.Split(strings.TrimPrefix(health, "http://"), "/")[0]
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("HTTP socket still accepts connections")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("listener probe timed out; closure unproven")
	}
}

func appMixWait(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(40 * time.Second)
	for time.Now().Before(until) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("App fixture timed out")
}
func appMixCount(t *testing.T, db *gorm.DB, status models.TaskStatus, site string) int64 {
	t.Helper()
	q := db.Model(&models.CrawlerTask{}).Where("status = ?", status)
	if site != "" {
		q = q.Where("site = ?", site)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// Only the parent allocates/drops; child DSNs always name this exclusive DB.
func appMixDB(t *testing.T) (*gorm.DB, string) { return appMixDatabase(t, true) }

func appMixDatabase(t *testing.T, seed bool) (*gorm.DB, string) {
	t.Helper()
	raw := os.Getenv("PAPA_COMPENSATION_TEST_DSN")
	if raw == "" {
		t.Skip("PAPA_COMPENSATION_TEST_DSN not set")
	}
	c, err := driver.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid test connection")
	}
	c.DBName = ""
	c.Timeout = 5 * time.Second
	c.ReadTimeout = 15 * time.Second
	c.WriteTimeout = 15 * time.Second
	c.ParseTime = true
	server, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test server")
	}
	t.Cleanup(func() { server.Close() })
	name := "papa_app_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	drop, err := testdb.Create(ctx, server, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Error(err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var n int
		if err := server.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&n); err != nil || n != 0 {
			t.Errorf("cleanup not confirmed for %s: %v count=%d", name, err, n)
		} else {
			t.Logf("cleanup verified: %s absent", name)
		}
	})
	c.DBName = name
	dsn := c.FormatDSN()
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open owned database")
	}
	sqldb, _ := db.DB()
	t.Cleanup(func() { sqldb.Close() })
	cfg := &config.Config{}
	cfg.Server.OperationLog = true
	if err := database.Migrate(db, cfg, &appMixEffect{}, &appMixAttempt{}, &appMixControl{}); err != nil {
		t.Fatal(err)
	}
	if !seed {
		return db, dsn
	}
	for _, site := range []string{"alpha", "beta"} {
		for _, stage := range []string{"catalog", "detail"} {
			for i := 0; i < 16; i++ {
				row := models.CrawlerTask{Site: site, Stage: stage, URL: fmt.Sprintf("https://example.invalid/%s/%d", stage, i), Status: models.TaskStatusPending}
				if i%4 == 0 {
					at := time.Now().Add(2 * time.Second)
					row.NotBefore = &at
				}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return db, dsn
}

func TestAppMySQLMixedLifecycle(t *testing.T) {
	for _, mode := range []string{"graceful", "cancel", "timeout", "crash"} {
		t.Run(mode, func(t *testing.T) {
			db, dsn := appMixDB(t)
			token := uuid.NewString()
			if err := db.Create(&appMixControl{ID: 1, Token: token}).Error; err != nil {
				t.Fatal(err)
			}
			var dir string
			// Register before TempDir so this runs after its own cleanup and child reaping.
			t.Cleanup(func() {
				if dir == "" {
					return
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("fixture directory cleanup unconfirmed: %v", err)
				} else {
					t.Log("fixture directory cleanup verified")
				}
			})
			dir = t.TempDir()
			start := func(mode string) (*exec.Cmd, <-chan error) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestAppMySQLMixedChild$", "-test.v", "-test.count=1", "-test.timeout=4m")
				cmd.Env = append(os.Environ(), "PAPA_APP_CHILD_DSN="+dsn, "PAPA_APP_CHILD_TOKEN="+token, "PAPA_APP_CHILD_MODE="+mode, "PAPA_APP_CHILD_DIR="+filepath.Join(dir, mode))
				var output bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = &output
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					err := cmd.Wait()
					if err != nil {
						err = fmt.Errorf("%w: %s", err, output.String())
					}
					done <- err
					close(done)
				}()
				t.Cleanup(func() {
					_ = cmd.Process.Kill()
					select {
					case <-done:
						t.Logf("child %s output:\n%s", mode, output.String())
					case <-time.After(10 * time.Second):
						t.Error("cleanup could not reap child")
					}
				})
				return cmd, done
			}
			cmd, done := start(mode)
			if mode == "crash" || mode == "timeout" {
				appMixWait(t, func() bool {
					select {
					case err := <-done:
						t.Fatalf("child exited before cut: %v", err)
					default:
					}
					var row appMixControl
					if err := db.First(&row, 1).Error; err != nil {
						t.Fatal(err)
					}
					if row.Ready && row.Retained != (mode == "timeout") {
						t.Fatal("cut resource-retention evidence missing")
					}
					return row.Ready
				})
				if appMixCount(t, db, models.TaskStatusProcessing, "") == 0 {
					t.Fatal("kill cut has no claimed task")
				}
				var cutTasks []models.CrawlerTask
				if err := db.Where("stage = ? AND status = ?", "catalog", models.TaskStatusProcessing).Find(&cutTasks).Error; err != nil {
					t.Fatal(err)
				}
				cutSites := map[string]bool{}
				for _, row := range cutTasks {
					cutSites[row.Site] = true
					var effect appMixEffect
					if err := db.First(&effect, "task_id = ?", row.ID).Error; err != nil || effect.Site != row.Site || effect.URL != row.URL {
						t.Fatal("catalog cut has no matching committed effect", err)
					}
				}
				if len(cutTasks) != 2 || !cutSites["alpha"] || !cutSites["beta"] {
					t.Fatal("kill cut did not cover both sites")
				}
				var effects int64
				if err := db.Model(&appMixEffect{}).Count(&effects).Error; err != nil || effects == 0 {
					t.Fatal("cut has no committed effects", err)
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil || strings.Contains(err.Error(), "DATA RACE") {
						t.Fatalf("invalid kill result: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("child not reaped")
				}
			} else {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(180 * time.Second):
					t.Fatal("App child exceeded deadline")
				}
			}
			var before []appMixAttempt
			if err := db.Find(&before).Error; err != nil {
				t.Fatal(err)
			}
			priorCounts := map[uint]int{}
			for _, row := range before {
				priorCounts[row.TaskID]++
			}
			var priorTasks []models.CrawlerTask
			if err := db.Find(&priorTasks).Error; err != nil {
				t.Fatal(err)
			}
			priorStatus := map[uint]models.TaskStatus{}
			unconfirmed := map[string]int{}
			for _, row := range priorTasks {
				priorStatus[row.ID] = row.Status
				if row.Status == models.TaskStatusProcessing {
					unconfirmed[row.Site+":"+row.Stage]++
				}
			}
			if mode == "cancel" {
				for _, row := range priorTasks {
					if row.Stage == "catalog" && priorCounts[row.ID] > 0 && row.Status != models.TaskStatusPending {
						t.Fatal("canceled catalog claim was not released")
					}
				}
			}
			if mode != "graceful" {
				if appMixCount(t, db, models.TaskStatusPending, "") == 0 {
					t.Fatal("interruption did not leave durable backlog")
				}
				_, done = start("recover")
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(90 * time.Second):
					t.Fatal("App recovery timed out")
				}
			}
			var control appMixControl
			if err := db.First(&control, 1).Error; err != nil {
				t.Fatal(err)
			}
			var tasks []models.CrawlerTask
			if err := db.Find(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			if len(tasks) != control.Total || control.Total < 96 || control.Total%6 != 0 {
				t.Fatalf("task manifest count %d/%d", len(tasks), control.Total)
			}
			groups := map[string]int{}
			manifest := map[string]bool{}
			var effects []appMixEffect
			if err := db.Find(&effects).Error; err != nil {
				t.Fatal(err)
			}
			byID := map[uint]appMixEffect{}
			for _, e := range effects {
				byID[e.TaskID] = e
			}
			if len(effects) != len(tasks) {
				t.Fatal("result count differs")
			}
			for _, task := range tasks {
				manifest[task.Site+":"+task.Stage+":"+task.URL] = true
				e := byID[task.ID]
				if task.Status != models.TaskStatusSuccess || e.Site != task.Site || e.Stage != task.Stage || e.URL != task.URL {
					t.Fatalf("task/result mismatch: %d", task.ID)
				}
				groups[task.Site+":"+task.Stage]++
				if task.NotBefore != nil && (task.LastStartedAt == nil || task.LastStartedAt.Before(*task.NotBefore)) {
					t.Fatal("executed before durable due time")
				}
			}
			if len(groups) != 4 || groups["alpha:catalog"]*2 != groups["alpha:detail"] || groups["alpha:catalog"] != groups["beta:catalog"] || groups["alpha:detail"] != groups["beta:detail"] {
				t.Fatalf("cross-site/stage manifest mismatch: %v", groups)
			}
			for _, site := range []string{"alpha", "beta"} {
				for i := 0; i < control.Total/6; i++ {
					for _, kind := range []string{"catalog", "detail", "child"} {
						stage := kind
						if kind == "child" {
							stage = "detail"
						}
						if !manifest[fmt.Sprintf("%s:%s:https://example.invalid/%s/%d", site, stage, kind, i)] {
							t.Fatal("missing original or derived manifest member")
						}
					}
				}
			}
			var attempts []appMixAttempt
			if err := db.Find(&attempts).Error; err != nil {
				t.Fatal(err)
			}
			due := map[uint]time.Time{}
			for _, task := range tasks {
				if task.NotBefore != nil {
					due[task.ID] = *task.NotBefore
				}
			}
			counts := map[uint]int{}
			for _, a := range attempts {
				counts[a.TaskID]++
				if a.StartedAt.IsZero() || a.StartedAt.Before(due[a.TaskID]) {
					t.Fatalf("attempt executed before durable deadline: task %d", a.TaskID)
				}
				if _, ok := byID[a.TaskID]; !ok {
					t.Fatal("orphan attempt")
				}
			}
			for _, task := range tasks {
				want := priorCounts[task.ID]
				status, existed := priorStatus[task.ID]
				if !existed || status != models.TaskStatusSuccess {
					want++
				}
				if counts[task.ID] != want {
					t.Fatalf("task %d replay mismatch: %d want %d", task.ID, counts[task.ID], want)
				}
				if counts[task.ID] < 1 || counts[task.ID] > 2 {
					t.Fatal("unexpected replay count")
				}
				if mode == "graceful" && counts[task.ID] != 1 {
					t.Fatal("replay without interruption")
				}
			}
			if mode != "graceful" && len(attempts) <= len(tasks) {
				t.Fatal("recovery did not replay interrupted work")
			}
			t.Logf("mode=%s tasks=%d attempts=%d before_restart=%d unconfirmed=%v groups=%v", mode, len(tasks), len(attempts), len(before), unconfirmed, groups)
		})
	}
}

func TestAppMySQLMixedChild(t *testing.T) {
	dsn := os.Getenv("PAPA_APP_CHILD_DSN")
	if dsn == "" {
		t.Skip("parent-owned child only")
	}
	c, err := driver.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^papa_app_test_[a-f0-9]{32}$`).MatchString(c.DBName) {
		t.Fatal("not an isolated App test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("child connection failed")
	}
	observer, _ := db.DB()
	defer observer.Close()
	var control appMixControl
	if err := db.First(&control, 1).Error; err != nil || control.Token == "" || control.Token != os.Getenv("PAPA_APP_CHILD_TOKEN") {
		t.Fatal("test ownership mismatch")
	}
	mode := os.Getenv("PAPA_APP_CHILD_MODE")
	if mode != "graceful" && mode != "recover" && mode != "cancel" && mode != "timeout" && mode != "crash" {
		t.Fatal("bad child mode")
	}
	dir := os.Getenv("PAPA_APP_CHILD_DIR")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	cfg := map[string]any{"app": map[string]any{"env": "test"}, "db": map[string]any{"driver": "mysql", "dsn": dsn, "log_level": "silent", "max_open_conns": 12, "max_idle_conns": 4}, "log": map[string]any{"dir": filepath.Join(dir, "logs"), "level": "error"}, "crawler": map[string]any{"stop_timeout": "100ms", "drain_interval": "20ms", "trace": map[string]any{"mode": "off"}}, "server": map[string]any{"enabled": true, "port": port, "operation_log": true, "shutdown_timeout": "2s"}, "recover_queue": map[string]any{"enabled": true, "worker_count": 1, "batch_size": 16, "batch_pause": "1ms"}, "obs": map[string]any{"events": map[string]any{"enabled": true}, "report": map[string]any{"on_exit": true, "dir": filepath.Join(dir, "reports")}}}
	cfg["browser"] = map[string]any{"enable": true, "pool_size": 1, "temp_dir": filepath.Join(dir, "browser"), "headless": true}
	cfg["obs"].(map[string]any)["report"].(map[string]any)["interval"] = "2s"
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := NewApp(WithConfigPath(path))
	if err != nil {
		t.Fatal("NewApp failed", err)
	}
	if err := a.Engine.SetSchedulerLimits(engine.SchedulerLimits{Global: 8, Site: 4, Stage: 2, RefillBatch: 4, DelayWindow: time.Second}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan uint, 4)
	lateWrite := make(chan struct{})
	lateResults := make(chan error, 2)
	var canceled atomic.Int32
	run := func(ctx context.Context, task *engine.Task, e *engine.Engine) error {
		if err := a.DB.Create(&appMixAttempt{TaskID: uint(task.ID), StartedAt: time.Now().UTC()}).Error; err != nil {
			return err
		}
		if task.Stage == "catalog" {
			child := &engine.Task{Site: task.Site, Stage: "detail", URL: strings.Replace(task.URL, "/catalog/", "/child/", 1), Delay: 100 * time.Millisecond}
			if err := e.SubmitTask(child); err != nil {
				return err
			}
		}
		if err := a.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&appMixEffect{TaskID: uint(task.ID), Site: task.Site, Stage: task.Stage, URL: task.URL}).Error; err != nil {
			return err
		}
		if task.Stage == "catalog" && mode != "graceful" && mode != "recover" {
			entered <- uint(task.ID)
			if mode == "cancel" {
				<-ctx.Done()
				canceled.Add(1)
				return ctx.Err()
			}
			if mode == "timeout" {
				<-lateWrite
				// Still inside the handler, after Run has returned partial.
				lateResults <- a.DB.Model(&appMixEffect{}).Where("task_id = ?", task.ID).Update("url", task.URL).Error
			}
			select {}
		}
		select {
		case <-time.After(5 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, site := range []string{"alpha", "beta"} {
		a.RegisterSites(SiteSpec{Key: site, StartPaused: mode == "graceful" && site == "alpha", Stages: []StageSpec{{Fetcher: appMixFetcher{"catalog", run}, WorkerCount: 1, QueueSize: 4, Retry: RetrySpec{MaxAttempts: 1}}, {Fetcher: appMixFetcher{"detail", run}, WorkerCount: 1, QueueSize: 4, Retry: RetrySpec{MaxAttempts: 1}}}})
	}
	ready := make(chan struct{})
	var once sync.Once
	held := make(chan struct{})
	release := make(chan struct{})
	var holdDone atomic.Bool
	a.UseRouter(func(r *Router) {
		r.NoAuth().Get("/synthetic/"+control.Token, func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(ready) }); w.WriteHeader(204) })
		r.NoAuth().Get("/synthetic-hold/"+control.Token, func(w http.ResponseWriter, r *http.Request) {
			close(held)
			<-release
			if err := a.DB.Exec("SELECT 1").Error; err != nil {
				w.WriteHeader(500)
				return
			}
			holdDone.Store(true)
			w.WriteHeader(204)
		})
	})
	var hookOK atomic.Bool
	a.UseShutdown(func(context.Context) error {
		if err := a.DB.Exec("SELECT 1").Error; err != nil {
			return err
		}
		if mode == "graceful" && !holdDone.Load() {
			return fmt.Errorf("HTTP not drained before hook")
		}
		if a.oplog == nil {
			return fmt.Errorf("audit recorder not assembled")
		}
		for i := 0; i < 16; i++ {
			a.oplog.RecordEvent(oplog.Event{Table: "synthetic", Action: "mix-" + mode, RowID: fmt.Sprint(i), Operator: "fixture"})
		}
		hookOK.Store(true)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	appMixWait(t, func() bool {
		res, err := client.Get(base + "/synthetic/" + control.Token)
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 204
	})
	<-ready
	if mode == "crash" || mode == "timeout" || mode == "cancel" {
		cutIDs := make([]uint, 0, 2)
		for i := 0; i < 2; i++ {
			select {
			case id := <-entered:
				cutIDs = append(cutIDs, id)
			case <-time.After(20 * time.Second):
				t.Fatal("both sites did not enter")
			}
		}
		if mode != "crash" {
			cancel()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("App shutdown did not return")
			}
			if !hookOK.Load() {
				t.Fatal("shutdown hook did not run")
			}
			sqldb, _ := a.DB.DB()
			if err := sqldb.Ping(); err != nil {
				t.Fatal("App closed DB despite unfinished work")
			}
			if mode == "timeout" {
				close(lateWrite)
				for i := 0; i < 2; i++ {
					select {
					case err := <-lateResults:
						if err != nil {
							t.Fatal("late handler lost shared DB", err)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("late handler write did not complete")
					}
				}
				t.Log("both live handlers wrote after Run returned partial")
			}
			appMixPoolClosed(t, a, false)
			appMixStopped(t, a, client, base+"/synthetic/"+control.Token)
			appMixArtifacts(t, dir, "partial")
			var auditCount int64
			if err := db.Model(&models.OperationLog{}).Where("action = ?", "mix-"+mode).Count(&auditCount).Error; err != nil || auditCount != 16 {
				t.Fatal("audit did not drain before Run returned", auditCount, err)
			}
			if mode == "cancel" && canceled.Load() != 2 {
				t.Fatal("handlers did not observe cancellation")
			}
			if mode == "cancel" {
				appMixWait(t, func() bool {
					var n int64
					if err := db.Model(&models.CrawlerTask{}).Where("id IN ? AND status = ?", cutIDs, models.TaskStatusPending).Count(&n).Error; err != nil {
						t.Fatal(err)
					}
					return n == 2
				})
			}
		}
		if err := db.Model(&appMixControl{}).Where("id = 1").Updates(map[string]any{"ready": true, "retained": mode != "crash"}).Error; err != nil {
			t.Fatal(err)
		}
		if mode == "cancel" {
			return
		}
		select {}
	}
	total := 96
	if mode == "graceful" {
		appMixWait(t, func() bool { return appMixCount(t, db, models.TaskStatusSuccess, "beta") == 48 })
		if appMixCount(t, db, models.TaskStatusSuccess, "alpha") != 0 {
			t.Fatal("paused site ran")
		}
		if appMixCount(t, db, models.TaskStatusPending, "alpha") != 32 || a.Engine.SchedulerResidentCount() > 8 {
			t.Fatal("paused durable backlog or global budget incorrect")
		}
		if !a.Engine.ResumeSite("alpha") {
			t.Fatal("resume refused")
		}
		duration := 3 * time.Second
		if raw := os.Getenv("PAPA_APP_MIX_DURATION"); raw != "" {
			duration, err = time.ParseDuration(raw)
			if err != nil || duration < time.Second || duration > time.Minute {
				t.Fatal("duration must be 1s..1m")
			}
		}
		deadline := time.Now().Add(duration)
		next := 16
		for time.Now().Before(deadline) {
			for _, site := range []string{"alpha", "beta"} {
				for _, stage := range []string{"catalog", "detail"} {
					for i := next; i < next+8; i++ {
						if err := a.Engine.SubmitTask(&engine.Task{Site: site, Stage: stage, URL: fmt.Sprintf("https://example.invalid/%s/%d", stage, i)}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			next += 8
			total += 48
			if a.Engine.SchedulerResidentCount() > 8 {
				t.Fatal("resident budget exceeded")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	appMixWait(t, func() bool {
		if a.Engine.SchedulerResidentCount() > 8 {
			t.Fatal("resident budget exceeded")
		}
		return appMixCount(t, db, models.TaskStatusSuccess, "") == int64(total) && a.Engine.SchedulerResidentCount() == 0
	})
	if mode == "graceful" {
		response := make(chan int, 1)
		go func() {
			res, err := client.Get(base + "/synthetic-hold/" + control.Token)
			if err != nil {
				response <- 0
				return
			}
			res.Body.Close()
			response <- res.StatusCode
		}()
		select {
		case <-held:
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP hold was not entered")
		}
		a.Shutdown()
		select {
		case <-done:
			t.Fatal("Run returned while HTTP was active")
		case <-time.After(80 * time.Millisecond):
		}
		if err := a.DB.Exec("SELECT 1").Error; err != nil {
			t.Fatal("DB closed before HTTP drain")
		}
		close(release)
		if <-response != 204 {
			t.Fatal("inflight HTTP lost DB")
		}
	} else {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("drained App did not exit")
	}
	if !hookOK.Load() {
		t.Fatal("hook not executed")
	}
	sqldb, _ := a.DB.DB()
	if sqldb.Ping() == nil {
		t.Fatal("drained App retained DB")
	}
	appMixPoolClosed(t, a, true)
	appMixStopped(t, a, client, base+"/synthetic/"+control.Token)
	appMixArtifacts(t, dir, "drained")
	var auditCount int64
	if err := db.Model(&models.OperationLog{}).Where("action = ?", "mix-"+mode).Count(&auditCount).Error; err != nil || auditCount != 16 {
		t.Fatal("audit did not drain before DB close", auditCount, err)
	}
	if err := db.Model(&appMixControl{}).Where("id = 1").Update("total", total).Error; err != nil {
		t.Fatal(err)
	}
}
