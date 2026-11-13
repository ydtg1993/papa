package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/account/gormstore"
	"log"
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestPoolStoreCASUsesStructuredKeysAndRevision(t *testing.T) {
	var output bytes.Buffer
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test@tcp(127.0.0.1:1)/test", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true, Logger: logger.New(log.New(&output, "", 0), logger.Config{LogLevel: logger.Info})})
	if err != nil {
		t.Fatal(err)
	}
	store := gormstore.NewGORMPoolStore(db)
	if err = store.SavePoolCAS(context.Background(), PoolState{PoolConfig: PoolConfig{Key: PoolKey{Site: "site", Name: "name"}}, Revision: 2}, 1); !errors.Is(err, ErrPoolConflict) {
		t.Fatalf("dry run must not claim publication: %v", err)
	}
	if err = store.SaveManagementCAS(context.Background(), ManagementState{Key: Key{Site: "site", ID: "id"}, Version: 3}, 2); !errors.Is(err, ErrPoolConflict) {
		t.Fatal(err)
	}
	sql := output.String()
	for _, want := range []string{"UPDATE `crawler_account_pools`", "pool_name = 'name' AND revision = 1", "UPDATE `crawler_account_management`", "account_id = 'id' AND revision = 2"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("CAS SQL missing %q: %s", want, sql)
		}
	}
}

func TestPoolDeletionTombstoneSurvivesJSON(t *testing.T) {
	// GORM uses JSON payloads; deletion must not be an omitted in-memory flag.
	s := PoolState{PoolConfig: PoolConfig{Key: PoolKey{Site: "s", Name: "p"}}, Revision: 9, Deleted: true}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"deleted":true`) {
		t.Fatal("tombstone not persisted")
	}
}
