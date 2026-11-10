package onboarding

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestProfileStoreCASUsesIdentityRevisionAndOpaqueReferences(t *testing.T) {
	var output bytes.Buffer
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test@tcp(127.0.0.1:1)/test", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true, Logger: logger.New(log.New(&output, "", 0), logger.Config{LogLevel: logger.Info})})
	if err != nil {
		t.Fatal(err)
	}
	store := NewGORMStore(db)
	err = store.SaveCAS(context.Background(), Profile{Key: account.Key{Site: "fixture", ID: "one"}, Revision: 2, Source: "oa", State: "ready", SecretRef: "opaque-ref", InputsRef: "input-ref"}, 1)
	if !errors.Is(err, ErrConflict) {
		t.Fatal("dry run claimed publication", err)
	}
	for _, want := range []string{"UPDATE `crawler_account_profiles`", "account_id = 'one' AND revision = 1", "`secret_ref`='opaque-ref'", "`inputs_ref`='input-ref'"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing %q: %s", want, output.String())
		}
	}
}

func TestProfileMySQLExplicitMigrationConcurrentCASAndRestart(t *testing.T) {
	dsn := os.Getenv("PAPA_ACCOUNT_ONBOARDING_MYSQL_DSN")
	if dsn == "" {
		path := os.Getenv("PAPA_RUNTIME_TEST_CONFIG")
		if path == "" {
			t.Skip("PAPA_ACCOUNT_ONBOARDING_MYSQL_DSN/PAPA_RUNTIME_TEST_CONFIG not configured")
		}
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal("cannot read test connection configuration")
		}
		dsn = cfg.DB.DSN
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test connection configuration")
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	server, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("cannot prepare test server")
	}
	defer server.Close()
	id, _ := randomID()
	name := "papa_onboarding_test_" + id[:24]
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
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("cannot open owned test database")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	store := NewGORMStore(db)
	if _, err = store.List(context.Background()); err == nil {
		t.Fatal("read succeeded before migration")
	}
	if db.Migrator().HasTable(&ProfileRecord{}) {
		t.Fatal("constructor/read created table")
	}
	if err = db.AutoMigrate(Models()...); err != nil {
		t.Fatal(err)
	}
	for expected := uint64(0); expected < 2; expected++ {
		var winners, conflicts atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p := Profile{Key: account.Key{Site: "fixture", ID: "cas"}, Revision: expected + 1, Source: "oa", State: "draft", Label: "Concurrent fixture"}
				err := store.SaveCAS(context.Background(), p, expected)
				if err == nil {
					winners.Add(1)
				} else if errors.Is(err, ErrConflict) {
					conflicts.Add(1)
				} else {
					t.Error("CAS failed")
				}
			}()
		}
		close(start)
		wg.Wait()
		if winners.Load() != 1 || conflicts.Load() != 11 {
			t.Fatalf("winners=%d conflicts=%d", winners.Load(), conflicts.Load())
		}
	}
	s, _, _ := newFixtureService(t, &fixtureProvider{}, store, 5*time.Second)
	p := draftFixture(t, s, "persisted")
	v := startFixture(t, s, p, "password", map[string]string{"subject": "alice", "password": "private-password"}, true)
	if got := confirmFixture(t, s, v.ID); got.State != "succeeded" {
		t.Fatal(got)
	}
	s.Close()
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	s2, err := New(Options{Manager: m, Profiles: NewGORMStore(db), Secrets: s.opts.Secrets})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_ = s2.RegisterProvider(&fixtureProvider{})
	if err = s2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Version(p.Key); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(context.Background(), p.Key)
	if err != nil || loaded.State != "ready" || loaded.SecretRef == "" || loaded.InputsRef == "" {
		t.Fatalf("durable profile %+v %v", loaded, err)
	}
	var row ProfileRecord
	if err = db.Where("site = ? AND account_id = ?", p.Key.Site, p.Key.ID).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.StateJSON, "private-password") || strings.Contains(row.StateJSON, "private-candidate-token") {
		t.Fatal("credential leaked into database")
	}
}
