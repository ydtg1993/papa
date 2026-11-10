package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	"github.com/ydtg1993/papa/v4/pkg/testdb"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestIdentityStoredRowsRejectCorruption(t *testing.T) {
	p := testProfile(1)
	raw, _ := json.Marshal(p)
	row := models.BrowserProfile{Site: p.Site, ProfileID: p.ID, Version: p.Version, SchemaVersion: p.SchemaVersion, ProfileJSON: string(raw)}
	if _, err := decodeProfile(row); err != nil {
		t.Fatal(err)
	}
	row.Version = 2
	if _, err := decodeProfile(row); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	row.ProfileJSON = "{"
	if _, err := decodeProfile(row); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	subject := Subject{Site: p.Site, Kind: "session", Key: "business-1"}
	ref := p.Reference()
	b := Binding{Subject: subject, Revision: 1, Active: &ref, State: "active"}
	raw, _ = json.Marshal(b)
	binding := models.BrowserIdentityBinding{Site: subject.Site, SubjectKind: subject.Kind, SubjectKey: subject.Key, Revision: 1, StateJSON: string(raw)}
	if _, err := decodeBinding(binding); err != nil {
		t.Fatal(err)
	}
	binding.Revision = 2
	if _, err := decodeBinding(binding); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

// Only an exclusively created disposable schema is migrated. The supplied DSN
// must name no database; the cleanup cannot adopt or drop any existing schema.
func TestIdentityMySQLMigrationCAS(t *testing.T) {
	dsn := os.Getenv("PAPA_IDENTITY_TEST_ADMIN_DSN")
	if dsn == "" && os.Getenv("PAPA_RUNTIME_TEST_CONFIG") != "" {
		connection, loadErr := config.Load(os.Getenv("PAPA_RUNTIME_TEST_CONFIG"))
		if loadErr != nil {
			t.Fatal("cannot read test connection configuration")
		}
		parsed, parseErr := mysqlDriver.ParseDSN(connection.DB.DSN)
		if parseErr != nil {
			t.Fatal("invalid test connection configuration")
		}
		parsed.DBName = ""
		parsed.Timeout = 5 * time.Second
		dsn = parsed.FormatDSN()
	}
	if dsn == "" {
		t.Skip("PAPA_IDENTITY_TEST_ADMIN_DSN not set")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil || cfg.DBName != "" {
		t.Fatal("identity integration requires an admin DSN with no database")
	}
	cfg.ParseTime = true
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("cannot open test admin connection")
	}
	defer admin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := fmt.Sprintf("papa_identity_test_%d", time.Now().UnixNano())
	cleanup, err := testdb.Create(ctx, admin, name)
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
		t.Fatal("cannot open owned identity test database")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	store := NewGORMStore(db)
	// Constructors and reads must not issue DDL.
	if _, err = store.ListBindings(ctx, ""); err == nil {
		t.Fatal("query unexpectedly succeeded before explicit migration")
	}
	if db.Migrator().HasTable(&models.BrowserIdentityBinding{}) {
		t.Fatal("query implicitly created a table")
	}
	if err = db.AutoMigrate(&models.BrowserProfile{}, &models.BrowserIdentityBinding{}); err != nil {
		t.Fatal(err)
	}
	var successes, conflicts atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.AppendProfile(ctx, testProfile(1), 0)
			if err == nil {
				successes.Add(1)
			} else if errors.Is(err, ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != 11 {
		t.Fatal("profile CAS admitted multiple writers", successes.Load(), conflicts.Load())
	}
	successes.Store(0)
	conflicts.Store(0)
	subject := Subject{Site: "fixture", Kind: "account", Key: "alice"}
	ref := testProfile(1).Reference()
	b := Binding{Subject: subject, Revision: 1, Pending: &ref, State: "pending"}
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := store.SaveBindingCAS(ctx, b, 0)
			if err == nil {
				successes.Add(1)
			} else if errors.Is(err, ErrConflict) {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != 11 {
		t.Fatal("binding CAS admitted multiple writers", successes.Load(), conflicts.Load())
	}
	active := b.Clone()
	active.Active = active.Pending
	active.Pending = nil
	active.State = "active"
	active.Revision = 2
	if err = store.SaveBindingCAS(ctx, active, 1); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveBindingCAS(ctx, active, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale binding CAS succeeded", err)
	}
	unknown := active.Clone()
	unknown.Revision = 3
	unknown.Pending = &identity.Reference{Site: "fixture", ID: "missing", Version: 1}
	unknown.State = "pending"
	if err = store.SaveBindingCAS(ctx, unknown, 2); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = db.Model(&models.BrowserIdentityBinding{}).Where("site = ?", subject.Site).Update("state_json", "{").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = store.ListBindings(ctx, ""); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}
