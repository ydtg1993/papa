package app

import (
	"bytes"
	"context"
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
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var loadGroups = []struct{ site, stage string }{
	{"alpha", "catalog"}, {"alpha", "detail"}, {"beta", "catalog"}, {"beta", "detail"},
}

const loadFailure = "intentional App assertion failure with active workers"
const loadSetupFailure = "intentional App assertion failure before Run"

// Each child runs a complete App. The parent owns its database and files.
func TestAppMySQLActiveDrain(t *testing.T)       { appLoadParent(t, "active") }
func TestAppMySQLSaturatedFairness(t *testing.T) { appLoadParent(t, "saturated") }

func appLoadParent(t *testing.T, mode string) {
	t.Helper()
	db, dsn := appMixDatabase(t, false)
	token := uuid.NewString()
	if err := db.Create(&appMixControl{ID: 1, Token: token}).Error; err != nil {
		t.Fatal(err)
	}
	var dir string
	t.Cleanup(func() {
		if dir != "" {
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("load directory cleanup failed: %v", err)
			} else {
				t.Log("load directory cleanup verified")
			}
		}
	})
	dir = t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAppMySQLLoadChild$", "-test.v", "-test.count=1", "-test.timeout=4m")
	cmd.Env = append(os.Environ(), "PAPA_APP_LOAD_DSN="+dsn, "PAPA_APP_LOAD_TOKEN="+token, "PAPA_APP_LOAD_DIR="+dir, "PAPA_APP_LOAD_MODE="+mode)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
			t.Logf("load child %s reaped:\n%s", mode, output.String())
		case <-time.After(10 * time.Second):
			t.Error("load child reap timeout")
		}
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("load child failed: %v\n%s", err, output.String())
		}
	case <-time.After(210 * time.Second):
		t.Fatal("load child timeout")
	}
	if mode == "failure" {
		t.Fatal("failure injection did not fail")
	}
	var control appMixControl
	if err := db.First(&control, 1).Error; err != nil {
		t.Fatal(err)
	}
	var tasks []models.CrawlerTask
	var effects []appMixEffect
	var attempts []appMixAttempt
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Find(&effects).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Find(&attempts).Error; err != nil {
		t.Fatal(err)
	}
	if control.Total < 4 || len(tasks) != control.Total || len(effects) != len(tasks) || len(attempts) != len(tasks) {
		t.Fatalf("manifest sizes: %d/%d/%d/%d", control.Total, len(tasks), len(effects), len(attempts))
	}
	byID := map[uint]appMixEffect{}
	counts := map[uint]int{}
	for _, e := range effects {
		byID[e.TaskID] = e
	}
	for _, a := range attempts {
		counts[a.TaskID]++
		if _, ok := byID[a.TaskID]; !ok {
			t.Fatal("orphan attempt")
		}
	}
	groups := map[string]int{}
	urls := map[string]bool{}
	for _, r := range tasks {
		e, ok := byID[r.ID]
		if !ok || r.Status != models.TaskStatusSuccess || r.LastStartedAt == nil || r.LastFinishedAt == nil || r.LastFinishedAt.Before(*r.LastStartedAt) || e.Site != r.Site || e.Stage != r.Stage || e.URL != r.URL || counts[r.ID] != 1 {
			t.Fatalf("settlement mismatch task %d status=%v attempts=%d", r.ID, r.Status, counts[r.ID])
		}
		key := r.Site + ":" + r.Stage
		groups[key]++
		urls[key+":"+r.URL] = true
	}
	if len(groups) != 4 {
		t.Fatalf("missing groups: %v", groups)
	}
	for _, g := range loadGroups {
		key := g.site + ":" + g.stage
		if groups[key] == 0 {
			t.Fatal("empty group", key)
		}
		for i := 0; i < groups[key]; i++ {
			if !urls[fmt.Sprintf("%s:https://example.invalid/load/%d", key, i)] {
				t.Fatal("missing submitted manifest member", key, i)
			}
		}
	}
	t.Logf("mode=%s tasks=%d effects=%d attempts=%d groups=%v", mode, len(tasks), len(effects), len(attempts), groups)
}

func TestAppMySQLFailureCleanup(t *testing.T) {
	if os.Getenv("PAPA_COMPENSATION_TEST_DSN") == "" {
		t.Skip("PAPA_COMPENSATION_TEST_DSN not set")
	}
	if mode := os.Getenv("PAPA_APP_LOAD_FAIL_OWNER"); mode != "" {
		appLoadParent(t, mode)
		return
	}
	for _, mode := range []string{"setup-failure", "failure"} {
		t.Run(mode, func(t *testing.T) { appLoadFailureCleanup(t, mode) })
	}
}

func appLoadFailureCleanup(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAppMySQLFailureCleanup$", "-test.v", "-test.count=1", "-test.timeout=75s")
	cmd.Env = append(os.Environ(), "PAPA_APP_LOAD_FAIL_OWNER="+mode)
	output, err := cmd.CombinedOutput()
	text := string(output)
	failureMarker, cleanupMarker := loadFailure, "load App cleanup verified"
	if mode == "setup-failure" {
		failureMarker, cleanupMarker = loadSetupFailure, "load pre-run App cleanup verified"
	}
	for _, marker := range []string{failureMarker, cleanupMarker, "load child " + mode + " reaped:", "load directory cleanup verified"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing failure cleanup evidence %q: %v\n%s", marker, err, text)
		}
	}
	if err == nil || ctx.Err() != nil || strings.Contains(text, "DATA RACE") || !regexp.MustCompile(`cleanup verified: papa_app_test_[a-f0-9]{32} absent`).Match(output) {
		t.Fatalf("invalid failure cleanup result: %v\n%s", err, text)
	}
	t.Logf("expected assertion failure and cleanup verified:\n%s", text)
}

func TestAppMySQLLoadChild(t *testing.T) {
	dsn := os.Getenv("PAPA_APP_LOAD_DSN")
	if dsn == "" {
		t.Skip("parent-owned child only")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^papa_app_test_[a-f0-9]{32}$`).MatchString(parsed.DBName) {
		t.Fatal("not an owned load database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("observer connection failed")
	}
	observer, _ := db.DB()
	defer observer.Close()
	var control appMixControl
	if err := db.First(&control, 1).Error; err != nil || control.Token == "" || control.Token != os.Getenv("PAPA_APP_LOAD_TOKEN") {
		t.Fatal("load ownership mismatch")
	}
	mode := os.Getenv("PAPA_APP_LOAD_MODE")
	if mode != "active" && mode != "saturated" && mode != "failure" && mode != "setup-failure" {
		t.Fatal("invalid load mode")
	}
	dir := os.Getenv("PAPA_APP_LOAD_DIR")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cfg := map[string]any{
		"app":           map[string]any{"env": "test"},
		"db":            map[string]any{"driver": "mysql", "dsn": dsn, "log_level": "silent", "max_open_conns": 12, "max_idle_conns": 4},
		"log":           map[string]any{"dir": filepath.Join(dir, "logs"), "level": "error"},
		"crawler":       map[string]any{"stop_timeout": "5s", "drain_interval": "10ms", "trace": map[string]any{"mode": "off"}},
		"server":        map[string]any{"enabled": true, "port": port, "operation_log": true, "shutdown_timeout": "2s"},
		"recover_queue": map[string]any{"enabled": false},
		"obs":           map[string]any{"events": map[string]any{"enabled": true}, "report": map[string]any{"on_exit": true, "interval": "2s", "dir": filepath.Join(dir, "reports")}},
		"browser":       map[string]any{"enable": true, "pool_size": 1, "headless": true, "temp_dir": filepath.Join(dir, "browser")},
	}
	fault := os.Getenv("PAPA_APP_OBS_FAULT")
	if fault == "off" {
		cfg["obs"].(map[string]any)["events"].(map[string]any)["enabled"] = false
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := NewApp(WithConfigPath(path))
	if err != nil {
		t.Fatal("NewApp", err)
	}
	// Setup assertions can fail before Run's cleanup is installed. NewApp
	// already owns a DB pool, event consumers and Engine's delay dispatcher.
	runStarted := false
	t.Cleanup(func() {
		if !runStarted {
			a.shutdownRun(false)
			if mode == "setup-failure" {
				sqlDB, err := a.DB.DB()
				if err != nil || sqlDB.Ping() == nil {
					t.Fatal("pre-run cleanup retained DB", err)
				}
				t.Log("load pre-run App cleanup verified")
			}
		}
	})
	if mode == "setup-failure" {
		t.Fatal(loadSetupFailure)
	}
	releaseArchive, archiveEntered := observationFault(t, a, fault)
	t.Cleanup(releaseArchive)
	if err := a.Engine.SetSchedulerLimits(engine.SchedulerLimits{Global: 4, Site: 3, Stage: 2, RefillBatch: 2}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 4)
	canceled := make(chan string, 4)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var countMu sync.Mutex
	completed := [4]int{}
	completedAt := [4]time.Time{}
	loadFetchers := make([]appMixFetcher, 4)
	for index, g := range loadGroups {
		index, g := index, g
		loadFetchers[index] = appMixFetcher{stage: g.stage, run: func(ctx context.Context, task *engine.Task, _ *engine.Engine) error {
			if err := a.DB.Create(&appMixAttempt{TaskID: uint(task.ID), StartedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
			if mode != "saturated" {
				entered <- g.site + ":" + g.stage
				<-ctx.Done()
				canceled <- g.site + ":" + g.stage
				<-release
			} else {
				select {
				case <-time.After(time.Duration(index+1) * 20 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if err := a.DB.Create(&appMixEffect{TaskID: uint(task.ID), Site: task.Site, Stage: task.Stage, URL: task.URL}).Error; err != nil {
				return err
			}
			countMu.Lock()
			completed[index]++
			completedAt[index] = time.Now()
			countMu.Unlock()
			return nil
		}}
	}
	for i, site := range []string{"alpha", "beta"} {
		a.RegisterSites(SiteSpec{Key: site, Stages: []StageSpec{
			{Fetcher: loadFetchers[i*2], WorkerCount: 1, QueueSize: 4, Retry: RetrySpec{MaxAttempts: 1}},
			{Fetcher: loadFetchers[i*2+1], WorkerCount: 1, QueueSize: 4, Retry: RetrySpec{MaxAttempts: 1}},
		}})
	}
	next := [4]int{}
	submit := func(group, n int) {
		g := loadGroups[group]
		for j := 0; j < n; j++ {
			if err := a.Engine.SubmitTask(&engine.Task{Site: g.site, Stage: g.stage, URL: fmt.Sprintf("https://example.invalid/load/%d", next[group])}); err != nil {
				t.Fatal(err)
			}
			next[group]++
		}
	}
	// Seed before Run: no stage may start before all four groups exist.
	for group, g := range loadGroups {
		n := 1
		if mode == "saturated" {
			n = 96
		}
		for i := 0; i < n; i++ {
			if err := db.Create(&models.CrawlerTask{Site: g.site, Stage: g.stage, URL: fmt.Sprintf("https://example.invalid/load/%d", i), Status: models.TaskStatusPending}).Error; err != nil {
				t.Fatal(err)
			}
		}
		next[group] = n
	}
	ready := make(chan struct{})
	var readyOnce sync.Once
	a.UseRouter(func(r *Router) {
		r.NoAuth().Get("/load/"+control.Token, func(w http.ResponseWriter, r *http.Request) {
			readyOnce.Do(func() { close(ready) })
			w.WriteHeader(204)
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runStarted = true
	go func() { defer close(done); a.Run(ctx) }()
	transport := &http.Transport{}
	client := &http.Client{Transport: transport, Timeout: time.Second}
	health := fmt.Sprintf("http://127.0.0.1:%d/load/%s", port, control.Token)
	// Fatal runs this cleanup too, while the process is still alive.
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			t.Error("cleanup App did not exit")
			return
		}
		transport.CloseIdleConnections()
		sqldb, _ := a.DB.DB()
		if sqldb.Ping() == nil {
			t.Fatal("cleanup retained DB")
		}
		appMixPoolClosed(t, a, true)
		appMixStopped(t, a, client, health)
		releaseArchive()
		if fault == "" {
			appMixArtifacts(t, dir, "drained")
		}
		if fault == "archive-blocked" || fault == "archive-panic" {
			if a.observation.archive.Dropped() == 0 {
				t.Fatal("archive fault did not record any lost events")
			}
			_ = signalReportJSON(t, a)
		}
		if fault == "report-unwritable" {
			if info, err := os.Stat(a.Config.Obs.Report.Dir); err != nil || info.IsDir() {
				t.Fatal("report failure fixture invalid", err)
			}
			if failures := a.reportRunner.Status().Failures; failures == 0 {
				t.Fatal("unwritable report never exercised a failed write")
			} else {
				t.Logf("unwritable report write failures=%d", failures)
			}
		}
		if fault == "off" {
			if _, err := os.Stat(a.Config.Obs.Report.Dir); !os.IsNotExist(err) {
				t.Fatal("disabled observer produced report", err)
			}
		}

		t.Log("load App cleanup verified")
	})
	appMixWait(t, func() bool {
		res, err := client.Get(health)
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 204
	})
	<-ready
	if archiveEntered != nil {
		select {
		case <-archiveEntered:
		case <-time.After(time.Second):
			t.Fatal("faulty archive was not exercised")
		}
	}
	if mode != "saturated" {
		seen := map[string]bool{}
		for i := 0; i < 4; i++ {
			select {
			case key := <-entered:
				seen[key] = true
			case <-time.After(10 * time.Second):
				t.Fatal("all active handlers did not enter")
			}
		}
		if len(seen) != 4 || appMixCount(t, db, models.TaskStatusProcessing, "") != 4 {
			t.Fatal("active cut does not have four processing groups")
		}
		if mode == "failure" {
			t.Fatal(loadFailure)
		}
		a.Shutdown()
		seen = map[string]bool{}
		for i := 0; i < 4; i++ {
			select {
			case key := <-canceled:
				seen[key] = true
			case <-time.After(3 * time.Second):
				t.Fatal("handler cancellation missing")
			}
		}
		if len(seen) != 4 {
			t.Fatal("cancellation did not cover all groups")
		}
		select {
		case <-done:
			t.Fatal("Run returned before active workers finished")
		case <-time.After(100 * time.Millisecond):
		}
		if err := a.DB.Exec("SELECT 1").Error; err != nil {
			t.Fatal("DB closed while workers active", err)
		}
		if appMixCount(t, db, models.TaskStatusProcessing, "") != 4 {
			t.Fatal("claims changed before handler result")
		}
		releaseOnce.Do(func() { close(release) })
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("active drain did not finish")
		}
		if appMixCount(t, db, models.TaskStatusSuccess, "") != 4 {
			t.Fatal("successful late handlers did not settle")
		}
		t.Log("four active groups observed cancellation, committed results and settled before Run returned")
	} else {
		duration := 10 * time.Second
		if raw := os.Getenv("PAPA_APP_SATURATION_DURATION"); raw != "" {
			duration, err = time.ParseDuration(raw)
			if err != nil || duration < 5*time.Second || duration > 2*time.Minute {
				t.Fatal("saturation duration must be 5s..2m")
			}
		}
		start := time.Now()
		last := [4]int{}
		// Sampling is independent of the producer's DB writes. A slow Submit
		// cannot hide a resident excursion or a starved group behind one sample.
		sampleStop, sampleDone := make(chan struct{}), make(chan struct{})
		var sampleOnce sync.Once
		var sampleErr string
		var maxGap time.Duration
		residentSamples, residentPeak := 0, 0
		go func() {
			defer close(sampleDone)
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-sampleStop:
					return
				case now := <-tick.C:
					r := a.Engine.SchedulerResidentCount()
					residentSamples++
					residentPeak = max(residentPeak, r)
					if r > 4 {
						sampleErr = fmt.Sprintf("resident budget exceeded: %d", r)
					}
					countMu.Lock()
					times := completedAt
					countMu.Unlock()
					for group, at := range times {
						if at.Before(start) {
							at = start
						}
						gap := now.Sub(at)
						maxGap = max(maxGap, gap)
						if gap > 5*time.Second {
							sampleErr = fmt.Sprintf("group %d starved for %s", group, gap)
						}
					}
				}
			}
		}()
		stopSampling := func() { sampleOnce.Do(func() { close(sampleStop) }); <-sampleDone }
		defer stopSampling()
		peakResident, minPending, samples := 0, 100000, 0
		for time.Since(start) < duration {
			resident := a.Engine.SchedulerResidentCount()
			peakResident = max(peakResident, resident)
			if resident > 4 {
				t.Fatal("global resident budget exceeded", resident)
			}
			countMu.Lock()
			progress := completed
			countMu.Unlock()
			for group, g := range loadGroups {
				var pending int64
				if err := db.Model(&models.CrawlerTask{}).Where("site = ? AND stage = ? AND status = ?", g.site, g.stage, models.TaskStatusPending).Count(&pending).Error; err != nil {
					t.Fatal(err)
				}
				minPending = min(minPending, int(pending))
				if pending < 8 {
					t.Fatalf("saturation lost in %s:%s pending=%d", g.site, g.stage, pending)
				}
				if progress[group] > last[group] {
					last[group] = progress[group]
				}
				if pending < 64 {
					submit(group, 96-int(pending))
				}
			}
			samples++
			time.Sleep(100 * time.Millisecond)
		}
		stopSampling()
		if sampleErr != "" {
			t.Fatal(sampleErr)
		}
		if residentSamples < 100 {
			t.Fatal("insufficient independent samples", residentSamples)
		}
		for group, n := range last {
			if n < 2 {
				t.Fatalf("group %d made insufficient progress: %d", group, n)
			}
		}
		if samples < 10 || peakResident != 4 {
			t.Fatalf("load not saturated: samples=%d peak=%d", samples, peakResident)
		}
		t.Logf("saturated duration=%s samples=%d min_group_pending=%d peak_resident=%d progress=%v submitted=%v", time.Since(start).Round(time.Millisecond), samples, minPending, peakResident, last, next)
		t.Logf("independent_samples=%d resident_peak=%d max_progress_gap=%s", residentSamples, residentPeak, maxGap)
		total := next[0] + next[1] + next[2] + next[3]
		appMixWait(t, func() bool {
			return appMixCount(t, db, models.TaskStatusSuccess, "") == int64(total) && a.Engine.SchedulerResidentCount() == 0
		})
		stopAt := time.Now()
		a.Shutdown()
		select {
		case <-done:
			if time.Since(stopAt) > 3*time.Second {
				t.Fatal("drained App observation shutdown exceeded 3s")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("saturated load drain timed out")
		}
	}
	if err := db.Model(&appMixControl{}).Where("id = 1").Update("total", next[0]+next[1]+next[2]+next[3]).Error; err != nil {
		t.Fatal(err)
	}
}
