package configstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionPolicyMySQLTransactions(t *testing.T) {
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
	name := "papa_execution_test_" + hex.EncodeToString(random)
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
	all := []any{&models.Config{}, &models.RuntimePolicyHistory{}, &models.ConfigOperation{}}
	if err = db.AutoMigrate(all...); err != nil {
		t.Fatal(err)
	}

	store := ExecutionStore{Store: New(db)}
	ctx := context.Background()
	doc := ep.InitialDocument()
	if _, err := store.Save(ctx, ep.Update{OperationID: "initialize-test", Document: doc}, "test"); err != nil {
		t.Fatal(err)
	}
	c1, c2 := ep.New(store, nil, nil), ep.New(store, nil, nil)
	for _, c := range []*ep.Controller{c1, c2} {
		if err := c.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var wins, conflicts atomic.Int32
	for i, c := range []*ep.Controller{c1, c2} {
		wg.Add(1)
		go func(i int, c *ep.Controller) {
			defer wg.Done()
			d := doc.Clone()
			d.Browser.PoolSize = 4 + i
			_, e := c.Update(ctx, ep.Update{ExpectedRevision: 1, OperationID: []string{"concurrent-one", "concurrent-two"}[i], Document: d}, "test")
			if e == nil {
				wins.Add(1)
			} else if errors.Is(e, ep.ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Error(e)
			}
		}(i, c)
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", wins.Load(), conflicts.Load())
	}
	h, err := store.Load(ctx)
	if err != nil || h.Revision != 2 {
		t.Fatalf("load: %+v %v", h, err)
	}
	// A failure after the config/history write must roll the entire transaction back.
	db.Callback().Create().Before("gorm:create").Register("execution_op_fail", func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_config_operations" {
			tx.AddError(errors.New("synthetic operation insert failure"))
		}
	})
	_, err = store.Save(ctx, ep.Update{ExpectedRevision: 2, OperationID: "rollback-failure", Document: doc}, "test")
	if err == nil {
		t.Fatal("expected atomic rollback")
	}
	db.Callback().Create().Remove("execution_op_fail")
	h, err = store.Load(ctx)
	if err != nil || h.Revision != 2 {
		t.Fatalf("partial commit: %+v %v", h, err)
	}
	if _, err := store.Operation(ctx, "rollback-failure"); !errors.Is(err, ep.ErrNotFound) {
		t.Fatalf("rolled back operation visible: %v", err)
	}
	hs, err := store.History(ctx, 100)
	if err != nil || len(hs) != 2 {
		t.Fatalf("history: %d %v", len(hs), err)
	}
	current := ep.New(store, nil, nil)
	if err := current.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	u := ep.Update{ExpectedRevision: 2, OperationID: "idempotent-retry", Document: doc, Reason: "test"}
	v, err := current.Update(ctx, u, "test")
	if err != nil || v.Revision != 3 {
		t.Fatalf("save: %+v %v", v, err)
	}
	v, err = current.Update(ctx, u, "test")
	if err != nil || v.Revision != 3 {
		t.Fatalf("retry: %+v %v", v, err)
	}
	u.Document.Browser.PoolSize++
	if _, err = current.Update(ctx, u, "test"); !errors.Is(err, ep.ErrConflict) {
		t.Fatalf("ID reuse: %v", err)
	}
	// MySQL's default collation folds case. It must not turn a different request
	// ID into a successful lookup of an old operation or a permanently pending write.
	if _, err = store.Operation(ctx, "IDEMPOTENT-RETRY"); !errors.Is(err, ep.ErrNotFound) {
		t.Fatalf("case-folded lookup: %v", err)
	}
	u.OperationID = "IDEMPOTENT-RETRY"
	u.ExpectedRevision = 3
	if _, err = current.Update(ctx, u, "test"); !errors.Is(err, ep.ErrConflict) {
		t.Fatalf("collated identity collision: %v", err)
	}
	h, err = store.Load(ctx)
	if err != nil || h.Revision != 3 {
		t.Fatalf("collision partially committed: %v %v", h, err)
	}
	u.OperationID = "fresh-after-collision"
	if _, err = current.Update(ctx, u, "test"); err != nil {
		t.Fatalf("collision wedged writes: %v", err)
	}
}
