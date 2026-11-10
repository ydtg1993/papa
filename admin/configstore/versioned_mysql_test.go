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
	all := append([]any{&models.Config{}, &models.CrawlerTask{}}, account.Models()...)
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
