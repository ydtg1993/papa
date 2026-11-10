package configstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRuntimePolicyMySQLConcurrentCAS(t *testing.T) {
	path := os.Getenv("PAPA_RUNTIME_TEST_CONFIG")
	if path == "" {
		t.Skip("PAPA_RUNTIME_TEST_CONFIG not configured; isolated database required")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal("cannot read test connection configuration")
	}
	dsn, err := driver.ParseDSN(cfg.DB.DSN)
	if err != nil {
		t.Fatal("invalid test connection configuration")
	}
	dsn.DBName = ""
	dsn.Timeout = 5 * time.Second
	dsn.ParseTime = true
	server, err := sql.Open("mysql", dsn.FormatDSN())
	if err != nil {
		t.Fatal("cannot prepare isolated database server")
	}
	defer server.Close()
	random := make([]byte, 8)
	if _, err = rand.Read(random); err != nil {
		t.Fatal(err)
	}
	name := "papa_runtime_test_" + hex.EncodeToString(random)
	cleanup, err := testdb.Create(context.Background(), server, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	dsn.DBName = name
	db, err := gorm.Open(mysql.Open(dsn.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open isolated database")
	}
	pool, _ := db.DB()
	defer pool.Close()
	pool.SetMaxOpenConns(16)
	all := append([]any{&models.Config{}, &models.CrawlerTask{}, &models.RuntimePolicyHistory{}}, account.Models()...)
	if err = db.AutoMigrate(all...); err != nil {
		t.Fatal(err)
	}
	store := New(db)
	for expected := uint64(0); expected < 2; expected++ {
		var winners, conflicts atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := store.SaveCAS(context.Background(), KeyRuntimePolicy, map[string]any{"schema_version": 1, "overrides": map[string]any{}}, expected, "fixture")
				if err == nil {
					winners.Add(1)
				} else if errors.Is(err, ErrConflict) {
					conflicts.Add(1)
				} else {
					t.Errorf("CAS: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if winners.Load() != 1 || conflicts.Load() != 11 {
			t.Fatalf("expected one winner: %d winners %d conflicts", winners.Load(), conflicts.Load())
		}
	}
	if _, rev, err := store.LoadVersioned(context.Background(), KeyRuntimePolicy); err != nil || rev != 2 {
		t.Fatalf("revision=%d err=%v", rev, err)
	}
	history, err := store.History(context.Background(), KeyRuntimePolicy)
	if err != nil || len(history) != 2 || history[0].Revision != 2 || history[1].Revision != 1 {
		t.Fatalf("history must contain only committed CAS winners: count=%d err=%v", len(history), err)
	}
	// A history write failure must roll back the policy update as well.
	if err := db.Exec("CREATE TRIGGER reject_history_fixture BEFORE INSERT ON crawler_runtime_policy_history FOR EACH ROW BEGIN IF NEW.revision = 3 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'history fixture failure'; END IF; END").Error; err != nil {
		t.Fatal(err)
	}
	_, failedWrite := store.SaveCAS(context.Background(), KeyRuntimePolicy, map[string]any{"schema_version": 1, "overrides": map[string]any{}}, 2, "failure-fixture")
	if failedWrite == nil {
		t.Fatal("policy committed without history")
	}
	if _, revision, err := store.LoadVersioned(context.Background(), KeyRuntimePolicy); err != nil || revision != 2 {
		t.Fatal("history failure advanced policy", revision, err)
	}
	if err := db.Exec("DROP TRIGGER reject_history_fixture").Error; err != nil {
		t.Fatal(err)
	}
	for expected := uint64(2); expected < 103; expected++ {
		if _, err := store.SaveCAS(context.Background(), KeyRuntimePolicy, map[string]any{"schema_version": 1, "overrides": map[string]any{}}, expected, "retention-fixture"); err != nil {
			t.Fatal(err)
		}
	}
	history, err = store.History(context.Background(), KeyRuntimePolicy)
	if err != nil || len(history) != RuntimeHistoryLimit || history[0].Revision != 103 || history[len(history)-1].Revision != 4 {
		t.Fatal("history retention must keep the newest committed versions", len(history), err)
	}
	if _, err = store.HistoryRevision(context.Background(), KeyRuntimePolicy, 3); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal("expired history still accessible", err)
	}
	if !db.Migrator().HasColumn(&models.CrawlerTask{}, "account_pool") {
		t.Fatal("missing account pool ownership column")
	}
	ps := account.NewGORMPoolStore(db)
	state := account.PoolState{PoolConfig: account.PoolConfig{Key: account.PoolKey{Site: "fixture", Name: "default"}, Members: []string{"first", "backup"}}, Revision: 1}
	if err = ps.SavePoolCAS(context.Background(), state, 0); err != nil {
		t.Fatal(err)
	}
	if err = ps.SavePoolCAS(context.Background(), state, 0); !errors.Is(err, account.ErrPoolConflict) {
		t.Fatalf("pool stale create: %v", err)
	}
	loaded, err := ps.LoadPools(context.Background())
	if err != nil || len(loaded) != 1 || loaded[0].Members[1] != "backup" {
		t.Fatal("pool persistence round-trip", err)
	}
}
