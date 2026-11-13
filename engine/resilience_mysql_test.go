package engine

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/internal/database"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// These tables are synthetic business fixtures, never framework migrations.
type resilienceEffect struct {
	TaskID uint `gorm:"primaryKey;autoIncrement:false"`
	Value  string
}
type resilienceAttempt struct {
	ID     uint `gorm:"primaryKey"`
	TaskID uint
}
type resilienceControl struct {
	ID         uint `gorm:"primaryKey"`
	Token      string
	Ready      bool
	Drained    bool
	StopMillis int64
	CutTaskID  uint
	StopCalled bool
	Unfinished int
}

func resilienceEngine(t *testing.T, db *gorm.DB, run func(context.Context, *Task, *Engine) error) *Engine {
	t.Helper()
	cfg := &config.Config{}
	cfg.Crawler.Trace.Mode = config.TraceOff
	cfg.Crawler.DrainInterval = 20 * time.Millisecond
	cfg.Crawler.QueueWatermark = .5
	cfg.RecoverQueue.Enabled = true
	cfg.RecoverQueue.BatchSize = 32
	cfg.RecoverQueue.WorkerCount = 1
	e := NewEngine(db, cfg, testLoggerSet())
	e.SetSiteBreaker("synthetic", breaker.Config{})
	if err := e.SetSchedulerLimits(SchedulerLimits{Global: 16, Site: 16, Stage: 16, RefillBatch: 16, DelayWindow: time.Second}); err != nil {
		t.Fatal(err)
	}
	e.stages["synthetic:detail"] = &stageInfo{stage: "detail", config: StageConfig{Site: "synthetic", WorkerCount: 2, QueueSize: 8, MaxAttempts: 1}, fetcher: resourceTestFetcher{run: run}}
	return e
}

func resilienceDB(t *testing.T) *gorm.DB {
	if os.Getenv("PAPA_COMPENSATION_TEST_DSN") == "" {
		t.Skip("PAPA_COMPENSATION_TEST_DSN not set")
	}
	// Register before the allocator: LIFO runs this after its DROP and Close.
	var name string
	t.Cleanup(func() {
		if name == "" {
			return
		}
		c, err := driver.ParseDSN(os.Getenv("PAPA_COMPENSATION_TEST_DSN"))
		if err != nil {
			t.Error("cannot verify test database cleanup")
			return
		}
		c.DBName = ""
		c.Timeout = 5 * time.Second
		server, err := sql.Open("mysql", c.FormatDSN())
		if err != nil {
			t.Error("cannot open cleanup verifier")
			return
		}
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var n int
		if err := server.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&n); err != nil {
			t.Error("cannot verify owned database absence")
			return
		}
		if n != 0 {
			t.Errorf("owned database remains: %s", name)
		} else {
			t.Logf("cleanup verified: %s absent", name)
		}
	})
	db, _ := compensationMySQL(t)
	if err := db.Raw("SELECT DATABASE()").Scan(&name).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(db, &config.Config{}, &resilienceEffect{}, &resilienceAttempt{}, &resilienceControl{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func resilienceBatch(t *testing.T, e *Engine, start, count int, delay time.Duration) {
	t.Helper()
	tasks := make([]*Task, 0, count)
	for i := start; i < start+count; i++ {
		task := &Task{Site: "synthetic", Stage: "detail", URL: fmt.Sprintf("https://example.invalid/resilience/%d", i), Meta: map[string]string{"value": strconv.Itoa(i)}}
		if i%4 == 0 {
			task.NotBefore = time.Now().Add(delay)
		}
		tasks = append(tasks, task)
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatal(err)
	}
}

func resilienceWrite(db *gorm.DB, task *Task) error {
	if err := db.Create(&resilienceAttempt{TaskID: uint(task.ID)}).Error; err != nil {
		return err
	}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&resilienceEffect{TaskID: uint(task.ID), Value: task.Meta["value"]}).Error
}

func resilienceWait(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("resilience condition timed out")
}

func resilienceComplete(t *testing.T, db *gorm.DB, want int) bool {
	t.Helper()
	var n int64
	if err := db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusSuccess).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n == int64(want)
}

func resilienceVerify(t *testing.T, db *gorm.DB, want int, replay map[uint]int64) {
	t.Helper()
	if err := resilienceCheck(db, want, replay); err != nil {
		t.Fatal(err)
	}
}

func resilienceCheck(db *gorm.DB, want int, replay map[uint]int64) error {
	var taskCount, effectCount int64
	expectedAttempts := int64(want)
	for _, n := range replay {
		if n != 1 {
			return fmt.Errorf("invalid interrupted attempt count: %d", n)
		}
		expectedAttempts += n
	}
	var attemptCount int64
	if err := db.Model(&resilienceAttempt{}).Count(&attemptCount).Error; err != nil {
		return err
	}
	if attemptCount != expectedAttempts {
		return fmt.Errorf("attempt total=%d want=%d", attemptCount, expectedAttempts)
	}
	replaySeen := 0
	if err := db.Model(&models.CrawlerTask{}).Count(&taskCount).Error; err != nil {
		return err
	}
	if err := db.Model(&resilienceEffect{}).Count(&effectCount).Error; err != nil {
		return err
	}
	if taskCount != int64(want) || effectCount != int64(want) {
		return fmt.Errorf("tasks/effects=%d/%d want=%d", taskCount, effectCount, want)
	}
	var distinct int64
	if err := db.Model(&models.CrawlerTask{}).Distinct("url").Count(&distinct).Error; err != nil {
		return err
	}
	if distinct != int64(want) {
		return fmt.Errorf("submitted URL set is not unique")
	}
	// Verification must not turn an opt-in multi-hour run into an unbounded read.
	var after uint
	for {
		var tasks []models.CrawlerTask
		if err := db.Where("id > ?", after).Order("id").Limit(256).Find(&tasks).Error; err != nil {
			return err
		}
		if len(tasks) == 0 {
			break
		}
		ids := make([]uint, 0, len(tasks))
		for _, task := range tasks {
			if replay[task.ID] > 0 {
				replaySeen++
			}
			ids = append(ids, task.ID)
		}
		var effects []resilienceEffect
		if err := db.Where("task_id IN ?", ids).Find(&effects).Error; err != nil {
			return err
		}
		values := map[uint]string{}
		for _, row := range effects {
			values[row.TaskID] = row.Value
		}
		var counts []struct {
			TaskID uint
			N      int64
		}
		if err := db.Model(&resilienceAttempt{}).Select("task_id, COUNT(*) AS n").Where("task_id IN ?", ids).Group("task_id").Scan(&counts).Error; err != nil {
			return err
		}
		attempts := map[uint]int64{}
		for _, row := range counts {
			attempts[row.TaskID] = row.N
		}
		for _, task := range tasks {
			n, err := strconv.Atoi(values[task.ID])
			if err != nil || n < 0 || n >= want || strconv.Itoa(n) != values[task.ID] {
				return fmt.Errorf("task %d outside submitted manifest", task.ID)
			}
			if attempts[task.ID] != 1+replay[task.ID] {
				return fmt.Errorf("task %d attempts=%d want=%d", task.ID, attempts[task.ID], 1+replay[task.ID])
			}
			if task.Status != models.TaskStatusSuccess || "https://example.invalid/resilience/"+values[task.ID] != task.URL {
				return fmt.Errorf("task %d lost terminal state or business value", task.ID)
			}
			if task.LastStartedAt == nil || task.LastFinishedAt == nil || task.LastFinishedAt.Before(*task.LastStartedAt) {
				return fmt.Errorf("task %d missing or reversed execution timestamps", task.ID)
			}
			if task.NotBefore != nil && task.LastStartedAt.Before(*task.NotBefore) {
				return fmt.Errorf("task %d executed before its durable deadline", task.ID)
			}
		}
		after = tasks[len(tasks)-1].ID
	}
	if replaySeen != len(replay) {
		return fmt.Errorf("interrupted attempt has no matching task")
	}
	return nil
}

// Counterexamples keep the acceptance oracle honest: counts alone must not
// hide a substituted task, an orphan result, or replay on the wrong task.
func TestResilienceMySQLRejectsCorruptEvidence(t *testing.T) {
	db := resilienceDB(t)
	e := resilienceEngine(t, db, func(_ context.Context, task *Task, _ *Engine) error { return resilienceWrite(db, task) })
	t.Cleanup(func() { e.Stop(5 * time.Second) })
	e.ApplyRegisterStage()
	resilienceBatch(t, e, 0, 4, 0)
	resilienceWait(t, 10*time.Second, func() bool { return resilienceComplete(t, db, 4) && e.SchedulerResidentCount() == 0 })
	if drained, _ := e.Stop(5 * time.Second); !drained {
		t.Fatal("baseline did not drain")
	}
	resilienceVerify(t, db, 4, nil)
	var rows []models.CrawlerTask
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	first, second := rows[0].ID, rows[1].ID
	cases := []struct {
		name, want string
		mutate     func(*gorm.DB) error
	}{
		{"substituted-task", "outside submitted manifest", func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE crawler_tasks SET url = ? WHERE id = ?", "https://example.invalid/resilience/99", first).Error; err != nil {
				return err
			}
			return tx.Model(&resilienceEffect{}).Where("task_id = ?", first).Update("value", "99").Error
		}},
		{"wrong-replay-owner", "attempts=", func(tx *gorm.DB) error {
			return tx.Model(&resilienceAttempt{}).Where("task_id = ?", first).Update("task_id", second).Error
		}},
		{"extra-orphan-attempt", "attempt total=", func(tx *gorm.DB) error { return tx.Create(&resilienceAttempt{TaskID: 999999}).Error }},
		{"orphan-result", "outside submitted manifest", func(tx *gorm.DB) error {
			return tx.Model(&resilienceEffect{}).Where("task_id = ?", first).Update("task_id", 999999).Error
		}},
		{"early-execution", "before its durable deadline", func(tx *gorm.DB) error {
			return tx.Exec("UPDATE crawler_tasks SET not_before = TIMESTAMPADD(SECOND, 60, UTC_TIMESTAMP(3)) WHERE id = ?", first).Error
		}},
		{"missing-terminal", "lost terminal state", func(tx *gorm.DB) error {
			return tx.Model(&models.CrawlerTask{}).Where("id = ?", first).Update("status", models.TaskStatusProcessing).Error
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer func() {
				if err := tx.Rollback().Error; err != nil {
					t.Error(err)
				}
			}()
			if err := tc.mutate(tx); err != nil {
				t.Fatal(err)
			}
			if err := resilienceCheck(tx, 4, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("corruption was not rejected at expected assertion: %v", err)
			}
		})
	}
	resilienceVerify(t, db, 4, nil)
}

func TestResilienceMySQLFailureCleanup(t *testing.T) {
	if os.Getenv("PAPA_COMPENSATION_TEST_DSN") == "" {
		t.Skip("PAPA_COMPENSATION_TEST_DSN not set")
	}
	const marker = "intentional assertion failure for cleanup verification"
	if os.Getenv("PAPA_RESILIENCE_FAIL_CLEANUP") == "1" {
		resilienceDB(t)
		t.Fatal(marker)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResilienceMySQLFailureCleanup$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "PAPA_RESILIENCE_FAIL_CLEANUP=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(output), marker) || !regexp.MustCompile(`cleanup verified: papa_compensation_test_[a-f0-9]{32} absent`).Match(output) || strings.Contains(string(output), "WARNING: DATA RACE") {
		t.Fatalf("assertion-failure cleanup was not verified: %v\n%s", err, output)
	}
	t.Logf("%s (after intentional assertion failure)", regexp.MustCompile(`cleanup verified: papa_compensation_test_[a-f0-9]{32} absent`).Find(output))
}

// The parent alone owns CREATE/DROP. Child writes are gated by a
// random database name AND an ownership token checked before any child write.
func TestSingleInstanceMySQLCrashRecovery(t *testing.T) {
	for _, cut := range []string{"before-effect", "after-effect", "stop-timeout"} {
		t.Run(cut, func(t *testing.T) {
			db := resilienceDB(t)
			token := uuid.NewString()
			if err := db.Create(&resilienceControl{ID: 1, Token: token}).Error; err != nil {
				t.Fatal(err)
			}
			seed := resilienceEngine(t, db, func(context.Context, *Task, *Engine) error { return fmt.Errorf("seed must stay paused") })
			t.Cleanup(func() { seed.Stop(time.Second) })
			if !seed.PauseSite("synthetic", "synthetic seed") {
				t.Fatal("seed pause refused")
			}
			resilienceBatch(t, seed, 0, 64, 5*time.Second)
			seed.Stop(time.Second)
			var pending int64
			if err := db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusPending).Count(&pending).Error; err != nil || pending != 64 {
				t.Fatalf("seed must have 64 pending tasks: %d, %v", pending, err)
			}
			var name string
			if err := db.Raw("SELECT DATABASE()").Scan(&name).Error; err != nil {
				t.Fatal(err)
			}
			c, err := driver.ParseDSN(os.Getenv("PAPA_COMPENSATION_TEST_DSN"))
			if err != nil {
				t.Fatal("invalid test DSN")
			}
			c.DBName = name
			c.ParseTime = true
			c.Timeout = 5 * time.Second
			start := func(mode string) (*exec.Cmd, chan error) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestResilienceMySQLChild$", "-test.count=1", "-test.timeout=90s")
				var output bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = &output
				cmd.Env = append(os.Environ(), "PAPA_RESILIENCE_CHILD_DSN="+c.FormatDSN(), "PAPA_RESILIENCE_CHILD_TOKEN="+token, "PAPA_RESILIENCE_CHILD_MODE="+mode)
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
				t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
				return cmd, done
			}
			cmd, done := start(cut)
			var control resilienceControl
			resilienceWait(t, 30*time.Second, func() bool {
				select {
				case err := <-done:
					t.Fatalf("child exited before cut: %v", err)
				default:
				}
				if err := db.First(&control, 1).Error; err != nil {
					t.Fatal(err)
				}
				return control.Ready
			})
			var processing int64
			if err := db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusProcessing).Count(&processing).Error; err != nil {
				t.Fatal(err)
			}
			if processing == 0 {
				t.Fatal("cut did not leave a claimed task")
			}
			var delayed int64
			if err := db.Model(&models.CrawlerTask{}).Where("status = ? AND not_before > UTC_TIMESTAMP(3)", models.TaskStatusPending).Count(&delayed).Error; err != nil || delayed == 0 {
				t.Fatalf("cut must retain durable future tasks: %d %v", delayed, err)
			}
			var cutTask models.CrawlerTask
			if control.CutTaskID == 0 {
				t.Fatal("cut has no task identity")
			}
			if err := db.First(&cutTask, control.CutTaskID).Error; err != nil || cutTask.Status != models.TaskStatusProcessing {
				t.Fatalf("cut task not processing: %v", err)
			}
			var cutEffects int64
			if err := db.Model(&resilienceEffect{}).Where("task_id = ?", control.CutTaskID).Count(&cutEffects).Error; err != nil {
				t.Fatal(err)
			}
			if (cut == "after-effect" && cutEffects != 1) || (cut != "after-effect" && cutEffects != 0) {
				t.Fatalf("wrong business commit cut: %s effects=%d", cut, cutEffects)
			}
			if cut == "stop-timeout" && (!control.StopCalled || control.Drained || control.Unfinished < 1 || control.StopMillis < 40 || control.StopMillis > 2000) {
				t.Fatalf("false drain or unbounded stop: %+v", control)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("child exited normally instead of being killed")
				}
				if strings.Contains(err.Error(), "WARNING: DATA RACE") {
					t.Fatalf("race before kill: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("kill did not reap child")
			}
			// Snapshot after reaping: no old process can append another attempt.
			var prior []resilienceAttempt
			if err := db.Find(&prior).Error; err != nil {
				t.Fatal(err)
			}
			replay := map[uint]int64{}
			for _, row := range prior {
				replay[row.TaskID]++
			}
			if replay[control.CutTaskID] != 1 {
				t.Fatal("cut task has no unique interrupted attempt")
			}
			_, recovered := start("recover")
			select {
			case err := <-recovered:
				if err != nil {
					t.Fatalf("recovery child failed: %v", err)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("restart timed out")
			}
			resilienceVerify(t, db, 64, replay)
			var attempts int64
			if err := db.Model(&resilienceAttempt{}).Count(&attempts).Error; err != nil {
				t.Fatal(err)
			}
			if attempts <= 64 || attempts > 66 {
				t.Fatalf("expected one or two interrupted handlers to replay: attempts=%d", attempts)
			}
			t.Logf("cut=%s processing_at_kill=%d delayed_at_kill=%d attempts=%d unique_effects=64 stop_ms=%d", cut, processing, delayed, attempts, control.StopMillis)
		})
	}
}

func TestResilienceMySQLChild(t *testing.T) {
	dsn := os.Getenv("PAPA_RESILIENCE_CHILD_DSN")
	if dsn == "" {
		t.Skip("parent-owned subprocess only")
	}
	c, err := driver.ParseDSN(dsn)
	if err != nil || !regexp.MustCompile(`^papa_compensation_test_[a-f0-9]{32}$`).MatchString(c.DBName) {
		t.Fatal("child database not isolated")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("child connection failed")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	var control resilienceControl
	if err := db.First(&control, 1).Error; err != nil || control.Token == "" || control.Token != os.Getenv("PAPA_RESILIENCE_CHILD_TOKEN") {
		t.Fatal("ownership token mismatch")
	}
	mode := os.Getenv("PAPA_RESILIENCE_CHILD_MODE")
	if mode != "recover" && mode != "before-effect" && mode != "after-effect" && mode != "stop-timeout" {
		t.Fatal("invalid cut")
	}
	entered := make(chan uint, 2)
	e := resilienceEngine(t, db, func(ctx context.Context, task *Task, _ *Engine) error {
		if mode == "recover" {
			return resilienceWrite(db, task)
		}
		if mode == "after-effect" {
			if err := resilienceWrite(db, task); err != nil {
				return err
			}
		} else if err := db.Create(&resilienceAttempt{TaskID: uint(task.ID)}).Error; err != nil {
			return err
		}
		entered <- uint(task.ID)
		select {} // Intentional noncooperation confined to a killable child.
	})
	e.ApplyRegisterStage()
	if mode == "recover" {
		defer func() {
			if t.Failed() {
				var rows []struct {
					Status int
					N      int64
				}
				db.Model(&models.CrawlerTask{}).Select("status, COUNT(*) AS n").Group("status").Scan(&rows)
				t.Logf("terminal diagnostic: %+v resident=%d", rows, e.SchedulerResidentCount())
				var failed models.CrawlerTask
				db.Where("status = ?", models.TaskStatusFailed).First(&failed)
				t.Logf("first failed: id=%d error=%s", failed.ID, failed.Error)
			}
		}()
		resilienceWait(t, 45*time.Second, func() bool { return resilienceComplete(t, db, 64) && e.SchedulerResidentCount() == 0 })
		if drained, _ := e.Stop(5 * time.Second); !drained {
			t.Fatal("recovered child did not drain")
		}
		return
	}
	var cutTaskID uint
	select {
	case cutTaskID = <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("handler not entered")
	}
	updates := map[string]any{"ready": true, "cut_task_id": cutTaskID}
	if mode == "stop-timeout" {
		start := time.Now()
		drained, stats := e.Stop(50 * time.Millisecond)
		updates["stop_called"] = true
		updates["unfinished"] = stats["synthetic:detail"].Unfinished
		updates["drained"] = drained
		updates["stop_millis"] = time.Since(start).Milliseconds()
	}
	if err := db.Model(&resilienceControl{}).Where("id = 1").Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
	select {}
}

// Opt-in sustained run; duration is explicit so short CI tests cannot be
// mistaken for a production soak. Uses only the fresh DB allocated above.
func TestSingleInstanceMySQLSoak(t *testing.T) {
	raw := os.Getenv("PAPA_RESILIENCE_SOAK_DURATION")
	if raw == "" {
		t.Skip("PAPA_RESILIENCE_SOAK_DURATION not set")
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration < time.Second || duration > 24*time.Hour {
		t.Fatal("invalid soak duration (1s..24h)")
	}
	db := resilienceDB(t)
	e := resilienceEngine(t, db, func(ctx context.Context, task *Task, _ *Engine) error {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		return resilienceWrite(db, task)
	})
	t.Cleanup(func() { e.Stop(5 * time.Second) })
	// Isolate continuous durable refill from the optional startup recovery pass.
	e.cfg.RecoverQueue.Enabled = false
	// Initial backlog is guaranteed to exceed the resident budget before start.
	if !e.PauseSite("synthetic", "synthetic initial backlog") {
		t.Fatal("initial pause refused")
	}
	resilienceBatch(t, e, 0, 64, 250*time.Millisecond)
	var initialPending int64
	if err := db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusPending).Count(&initialPending).Error; err != nil || initialPending != 64 {
		t.Fatalf("initial pending=%d %v", initialPending, err)
	}
	e.ApplyRegisterStage()
	if !e.ResumeSite("synthetic") {
		t.Fatal("initial resume refused")
	}
	start := time.Now()
	total := 64
	peakResident := 0
	peakPending := initialPending
	peakG := 0
	var peakHeap uint64
	nextBatch := start.Add(2 * time.Second)
	nextLog := start.Add(30 * time.Second)
	lastProgress := start
	var completed int64
	for time.Since(start) < duration {
		if time.Now().After(nextBatch) {
			resilienceBatch(t, e, total, 64, 250*time.Millisecond)
			total += 64
			nextBatch = time.Now().Add(2 * time.Second)
		}
		r := e.SchedulerResidentCount()
		if r > 16 {
			t.Fatalf("resident budget exceeded: %d", r)
		}
		peakResident = max(peakResident, r)
		var pending int64
		if err := db.Model(&models.CrawlerTask{}).Where("status IN ?", []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}).Count(&pending).Error; err != nil {
			t.Fatal(err)
		}
		peakPending = max(peakPending, pending)
		var success int64
		if err := db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusSuccess).Count(&success).Error; err != nil {
			t.Fatal(err)
		}
		if success > completed || pending == 0 {
			lastProgress = time.Now()
		}
		completed = success
		if pending > 0 && time.Since(lastProgress) > 10*time.Second {
			t.Fatalf("durable refill made no progress for 10s: success=%d pending=%d", success, pending)
		}
		peakG = max(peakG, runtime.NumGoroutine())
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		peakHeap = max(peakHeap, mem.HeapAlloc)
		if time.Now().After(nextLog) {
			t.Logf("elapsed=%s submitted=%d pending=%d resident=%d heap=%d goroutines=%d", time.Since(start).Round(time.Second), total, pending, r, mem.HeapAlloc, runtime.NumGoroutine())
			nextLog = time.Now().Add(30 * time.Second)
		}
		time.Sleep(50 * time.Millisecond)
	}
	resilienceWait(t, 60*time.Second, func() bool { return resilienceComplete(t, db, total) && e.SchedulerResidentCount() == 0 })
	if drained, _ := e.Stop(5 * time.Second); !drained {
		t.Fatal("soak did not drain")
	}
	resilienceVerify(t, db, total, nil)
	var attempts int64
	if err := db.Model(&resilienceAttempt{}).Count(&attempts).Error; err != nil {
		t.Fatal(err)
	}
	if attempts != int64(total) {
		t.Fatalf("unexpected replay without crash: %d/%d", attempts, total)
	}
	t.Logf("duration=%s tasks=%d attempts=%d peak_pending=%d peak_resident=%d peak_heap=%d peak_goroutines=%d", time.Since(start).Round(time.Millisecond), total, attempts, peakPending, peakResident, peakHeap, peakG)
}
