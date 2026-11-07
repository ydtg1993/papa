package agentstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Integration tests require their exact disposable database and only append
// unique test rows. No business database, automatic fallback or server startup.
func mysqlStore(t *testing.T) (*MySQLStore, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("PAPA_AGENT_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_AGENT_MYSQL_TEST_DSN not set; real MySQL integration not run")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_agent_test" {
		t.Fatal("integration DSN requires dedicated papa_agent_test database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqldb, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqldb.Close() })
	if err := db.AutoMigrate(&models.AgentRun{}, &models.AgentStep{}); err != nil {
		t.Fatal(err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db, uuid.NewString()
}

func TestMySQLStoreLifecycleCASCancelAndReopen(t *testing.T) {
	s, db, prefix := mysqlStore(t)
	exerciseStore(t, s, prefix)
	exerciseInterruptAndCancel(t, s, prefix)
	exerciseCAS(t, s, prefix)
	ctx := context.Background()
	r, err := s.Start(ctx, Run{ID: prefix + "-reopen", SnapshotJSON: `{"evidence_id":"approved-1"}`})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err = s.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{"calls":1}`)
	if err != nil {
		t.Fatal(err)
	}
	// A distinct connection/store must see the pending action and its reservation.
	reopenedDB, err := gorm.Open(mysql.Open(os.Getenv("PAPA_AGENT_MYSQL_TEST_DSN")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqldb, _ := reopenedDB.DB()
	defer sqldb.Close()
	reopened, err := New(reopenedDB)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := reopened.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(restored, r) {
		t.Fatalf("durable reopen: %+v %v", restored, err)
	}
	steps, err := reopened.Steps(ctx, r.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Status != StepPending {
		t.Fatalf("pending reopen: %+v %v", steps, err)
	}
	if _, err := reopened.Interrupt(ctx, r.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
	// Binary IDs remain case-sensitive in the actual schema.
	if _, err := s.Start(ctx, Run{ID: prefix + "-CASE"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, Run{ID: prefix + "-case"}); err != nil {
		t.Fatal(err)
	}
	// The store must detach inherited GORM WHERE clauses.
	scoped, err := New(db.Where("1 = 0"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Get(ctx, r.ID); err != nil {
		t.Fatalf("inherited caller scope: %v", err)
	}
}

var errInjectedWrite = errors.New("injected audit statement failure")

func TestMySQLBeginEndFailureRollsBackStepAndReservation(t *testing.T) {
	s, db, prefix := mysqlStore(t)
	ctx := context.Background()
	r, err := s.Start(ctx, Run{ID: prefix, UsageJSON: `{"calls":0}`})
	if err != nil {
		t.Fatal(err)
	}
	name := "agentstore_fail_step_insert"
	if err := db.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_agent_steps" {
			tx.AddError(errInjectedWrite)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, _, beginErr := s.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{"calls":1}`)
	db.Callback().Create().Remove(name)
	if !errors.Is(beginErr, errInjectedWrite) {
		t.Fatalf("injection: %v", beginErr)
	}
	after, err := s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(r, after) {
		t.Fatalf("half reservation persisted: %+v %v", after, err)
	}
	steps, err := s.Steps(ctx, r.ID, 0)
	if err != nil || len(steps) != 0 {
		t.Fatalf("step persisted after rollback: %+v %v", steps, err)
	}
	r, step, err := s.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{"calls":1}`)
	if err != nil {
		t.Fatal(err)
	}
	name = "agentstore_fail_step_update"
	if err := db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_agent_steps" {
			tx.AddError(errInjectedWrite)
		}
	}); err != nil {
		t.Fatal(err)
	}
	step.Status = StepSucceeded
	_, endErr := s.EndStep(ctx, r.ID, r.Revision, step, `{"calls":1,"tokens":20}`)
	db.Callback().Update().Remove(name)
	if !errors.Is(endErr, errInjectedWrite) {
		t.Fatalf("end injection: %v", endErr)
	}
	after, err = s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(r, after) {
		t.Fatalf("half result persisted: %+v %v", after, err)
	}
	steps, err = s.Steps(ctx, r.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Status != StepPending || steps[0].FinishedAt != nil {
		t.Fatalf("partial end persisted: %+v %v", steps, err)
	}
	// RowsAffected is a correctness boundary even if a callback reports no SQL error.
	name = "agentstore_zero_rows"
	if err := db.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_agent_runs" {
			tx.RowsAffected = 0
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, usageErr := s.UpdateUsage(ctx, r.ID, r.Revision, `{"calls":99}`)
	db.Callback().Update().Remove(name)
	if !errors.Is(usageErr, ErrConflict) {
		t.Fatalf("unchecked RowsAffected: %v", usageErr)
	}
	after, err = s.Get(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(r, after) {
		t.Fatalf("zero affected update persisted: %+v %v", after, err)
	}
}

var errLostCommitAck = errors.New("injected lost audit COMMIT acknowledgement")

type lostAckPool struct {
	gorm.ConnPool
	armed *atomic.Bool
}

func (p *lostAckPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.ConnPool.(*sql.DB).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lostAckTx{Tx: tx, armed: p.armed}, nil
}

type lostAckTx struct {
	*sql.Tx
	armed *atomic.Bool
}

func (t *lostAckTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	if t.armed.Swap(false) {
		return errLostCommitAck
	}
	return nil
}

func TestMySQLUnknownCommitNeverAuthorizesExternalCall(t *testing.T) {
	s, db, prefix := mysqlStore(t)
	ctx := context.Background()
	r, err := s.Start(ctx, Run{ID: prefix})
	if err != nil {
		t.Fatal(err)
	}
	sqldb, _ := db.DB()
	var armed atomic.Bool
	pool := &lostAckPool{ConnPool: sqldb, armed: &armed}
	wrapped := db.Session(&gorm.Session{NewDB: true})
	wrapped.Config.ConnPool = pool
	wrapped.Statement.ConnPool = pool
	uncertain, err := New(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	result, step, err := uncertain.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{"calls":1}`)
	externalCalls := 0
	if err == nil {
		externalCalls++
	}
	if !errors.Is(err, errLostCommitAck) || externalCalls != 0 || result.ID != "" || step.RunID != "" {
		t.Fatalf("unknown commit authorized work: %+v %+v %v", result, step, err)
	}
	persisted, err := s.Get(ctx, r.ID)
	if err != nil || persisted.Revision != 2 || persisted.UsageJSON != `{"calls":1}` {
		t.Fatalf("expected committed reservation: %+v %v", persisted, err)
	}
	steps, err := s.Steps(ctx, r.ID, 0)
	if err != nil || len(steps) != 1 || steps[0].Status != StepPending {
		t.Fatalf("missing uncertain step: %+v %v", steps, err)
	}
	// No replay even after a read discovers the commit. Only explicit interruption.
	if _, _, err := s.BeginStep(ctx, r.ID, persisted.Revision, Step{Kind: "model"}, `{}`); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncertain step replay: %v", err)
	}
	if _, err := s.Interrupt(ctx, r.ID, persisted.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLConstructorAndCorruptRecords(t *testing.T) {
	s, db, prefix := mysqlStore(t)
	ctx := context.Background()
	if _, err := New(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil: %v", err)
	}
	if _, err := New(db.Session(&gorm.Session{DryRun: true})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dry run: %v", err)
	}
	for _, prepared := range []bool{false, true} {
		session := db.Session(&gorm.Session{PrepareStmt: prepared})
		if _, err := New(session); err != nil {
			t.Fatalf("ordinary prepared=%v: %v", prepared, err)
		}
		tx := session.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		_, err := New(tx)
		tx.Rollback()
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("caller transaction prepared=%v: %v", prepared, err)
		}
	}
	r, err := s.Start(ctx, Run{ID: prefix, SnapshotJSON: `"` + strings.Repeat("x", MaxSnapshotBytes-2) + `"`, UsageJSON: `"` + strings.Repeat("u", MaxJSONBytes-2) + `"`})
	if err != nil {
		t.Fatalf("allowed sizes do not fit schema: %v", err)
	}
	if err := db.Model(&models.AgentRun{}).Where("id = ?", r.ID).Update("usage_json", `not-json`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, r.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt read: %v", err)
	}
	if _, err := s.UpdateUsage(ctx, r.ID, r.Revision, `{}`); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt mutation: %v", err)
	}
}

func TestMySQLCancelWhileWaitingForRunLock(t *testing.T) {
	s, db, prefix := mysqlStore(t)
	r, err := s.Start(context.Background(), Run{ID: prefix})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	var row models.AgentRun
	if err := tx.Raw("SELECT * FROM crawler_agent_runs WHERE id = ? FOR UPDATE", r.ID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = s.BeginStep(ctx, r.ID, r.Revision, Step{Kind: "model"}, `{"calls":1}`)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait cancellation: %v", err)
	}
	if err := tx.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(context.Background(), r.ID)
	if err != nil || !reflect.DeepEqual(r, after) {
		t.Fatalf("canceled lock wait reserved usage: %+v %v", after, err)
	}
	steps, err := s.Steps(context.Background(), r.ID, 0)
	if err != nil || len(steps) != 0 {
		t.Fatalf("canceled lock wait wrote step: %+v %v", steps, err)
	}
}
