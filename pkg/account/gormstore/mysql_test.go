package gormstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account/contract"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMySQLPoolAdapterExplicitMigrationCASAndRestart(t *testing.T) {
	path := os.Getenv("PAPA_RUNTIME_TEST_CONFIG")
	if path == "" {
		t.Skip("PAPA_RUNTIME_TEST_CONFIG not configured")
	}
	appConfig, err := config.Load(path)
	if err != nil {
		t.Fatal("cannot load test connection configuration")
	}
	cfg, err := driver.ParseDSN(appConfig.DB.DSN)
	if err != nil {
		t.Fatal("invalid test connection configuration")
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	server, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test database server")
	}
	defer server.Close()
	name := "papa_account_pool_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	cleanup, err := testdb.Create(context.Background(), server, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	cfg.DBName = name
	open := func() *gorm.DB {
		db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal("cannot open owned test database")
		}
		return db
	}
	db := open()
	conn, _ := db.DB()
	defer conn.Close()
	store := NewGORMPoolStore(db)
	ctx := context.Background()
	if _, err := store.LoadPools(ctx); err == nil {
		t.Fatal("read succeeded before migration")
	}
	if db.Migrator().HasTable(&PoolRecord{}) {
		t.Fatal("constructor/read created a table")
	}
	if err := db.AutoMigrate(Models()...); err != nil {
		t.Fatal("explicit migration failed")
	}
	for expected := uint64(0); expected < 2; expected++ {
		var winners, conflicts atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				value := contract.PoolState{PoolConfig: contract.PoolConfig{Key: contract.PoolKey{Site: "fixture", Name: "main"}, Members: []string{"one"}}, Revision: expected + 1, Deleted: expected == 1}
				err := store.SavePoolCAS(ctx, value, expected)
				if err == nil {
					winners.Add(1)
				} else if errors.Is(err, contract.ErrPoolConflict) {
					conflicts.Add(1)
				} else {
					t.Error("pool CAS failed")
				}
			}()
		}
		close(start)
		wg.Wait()
		if winners.Load() != 1 || conflicts.Load() != 7 {
			t.Fatalf("CAS winners=%d conflicts=%d", winners.Load(), conflicts.Load())
		}
	}
	management := contract.ManagementState{Key: contract.Key{Site: "fixture", ID: "one"}, Version: 1, Disabled: true, Events: []contract.HealthEvent{{Kind: "disabled"}}}
	if err := store.SaveManagementCAS(ctx, management, 0); err != nil {
		t.Fatal("management create failed")
	}
	if err := store.SaveManagementCAS(ctx, management, 0); !errors.Is(err, contract.ErrPoolConflict) {
		t.Fatal("management duplicate creation accepted")
	}
	management.Version = 2
	if err := store.SaveManagementCAS(ctx, management, 1); err != nil {
		t.Fatal("management update failed")
	}
	if err := store.SaveManagementCAS(ctx, management, 1); !errors.Is(err, contract.ErrPoolConflict) {
		t.Fatal("management stale update accepted")
	}
	if err := conn.Close(); err != nil {
		t.Fatal("cannot close first connection")
	}
	restarted := open()
	restartedConn, _ := restarted.DB()
	defer restartedConn.Close()
	restored := NewGORMPoolStore(restarted)
	pools, err := restored.LoadPools(ctx)
	if err != nil || len(pools) != 1 || pools[0].Revision != 2 || !pools[0].Deleted || len(pools[0].Members) != 1 {
		t.Fatalf("pool restart state mismatch: count=%d error=%v", len(pools), err)
	}
	states, err := restored.LoadManagement(ctx)
	if err != nil || len(states) != 1 || states[0].Version != 2 || !states[0].Disabled || len(states[0].Events) != 1 {
		t.Fatalf("management restart state mismatch: count=%d error=%v", len(states), err)
	}
}
