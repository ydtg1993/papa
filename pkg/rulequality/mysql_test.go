package rulequality

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
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func qualityMySQL(t *testing.T) (*MySQLStore, *gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("PAPA_QUALITY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("PAPA_QUALITY_MYSQL_TEST_DSN not set; real MySQL verification not run")
	}
	parsed, err := driver.ParseDSN(dsn)
	if err != nil || parsed.DBName != "papa_quality_test" {
		t.Fatal("requires dedicated papa_quality_test database")
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
	if err := db.AutoMigrate(&models.RuleObservation{}, &models.RuleQualityReport{}, &models.RuleReanalysisRequest{}); err != nil {
		t.Fatal(err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return s, db, uuid.NewString()
}
func TestQualityMySQLImmutableLifecycleCASAndReopen(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	exerciseQualityStore(t, s, prefix)
	exerciseQualityCAS(t, s, prefix+"-cas")
	ctx := context.Background()
	r, err := s.Report(ctx, prefix+"-report")
	if err != nil {
		t.Fatal(err)
	}
	db2, err := gorm.Open(mysql.Open(os.Getenv("PAPA_QUALITY_MYSQL_TEST_DSN")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db2.DB()
	defer sqlDB.Close()
	reopen, err := New(db2)
	if err != nil {
		t.Fatal(err)
	}
	read, err := reopen.Report(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(r, read) {
		t.Fatalf("durable report: %+v %v", read, err)
	}
	if request, err := reopen.Request(ctx, prefix+"-req"); err != nil || request.Status != AwaitingReview {
		t.Fatalf("durable request: %+v %v", request, err)
	}
	scoped, err := New(db.Where("1 = 0").Table("wrong_caller_table").Limit(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Report(ctx, r.ID); err != nil {
		t.Fatalf("inherited scope leaked: %v", err)
	}
	for _, suffix := range []string{"-CASE", "-case"} {
		o := storeObservation(prefix + suffix)
		if _, err := s.Record(ctx, o); err != nil {
			t.Fatalf("binary ID: %v", err)
		}
	}
}
func TestQualityMySQLConstructorRejectsTransactionAndDryRun(t *testing.T) {
	_, db, _ := qualityMySQL(t)
	if _, err := New(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil: %v", err)
	}
	if _, err := New(db.Session(&gorm.Session{DryRun: true})); !errors.Is(err, ErrInvalid) {
		t.Fatalf("dryrun: %v", err)
	}
	tx := db.Begin()
	defer tx.Rollback()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	if _, err := New(tx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("transaction: %v", err)
	}
}

var qualityInjectedError = errors.New("injected quality statement failure")

func TestQualityMySQLStatementFailureRollsBack(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	ctx := context.Background()
	o := storeObservation(prefix)
	name := "quality_fail_payload_finalize"
	if err := db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_rule_observations" {
			tx.AddError(qualityInjectedError)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Record(ctx, o)
	db.Callback().Update().Remove(name)
	if !errors.Is(err, qualityInjectedError) {
		t.Fatalf("injection: %v", err)
	}
	var count int64
	if err := db.Model(&models.RuleObservation{}).Where("id = ?", o.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial observation committed: %d %v", count, err)
	}
	report, err := s.SaveReport(ctx, storeReport(prefix+"-report", o.Scope))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, storeRequest(prefix, report.ID))
	if err != nil {
		t.Fatal(err)
	}
	name = "quality_zero_update"
	if err := db.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_rule_reanalysis_requests" {
			tx.RowsAffected = 0
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.StartRequest(ctx, r.ID, r.Revision, strings.Repeat("a", 64))
	db.Callback().Update().Remove(name)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("RowsAffected unchecked: %v", err)
	}
	after, err := s.Request(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(after, r) {
		t.Fatalf("failed start committed: %+v %v", after, err)
	}
}

var qualityLostAck = errors.New("injected lost quality COMMIT acknowledgment")

type qualityLostAckPool struct {
	gorm.ConnPool
	armed *atomic.Bool
}

func (p *qualityLostAckPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.ConnPool.(*sql.DB).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &qualityLostAckTx{Tx: tx, armed: p.armed}, nil
}

type qualityLostAckTx struct {
	*sql.Tx
	armed *atomic.Bool
}

func (t *qualityLostAckTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	if t.armed.Swap(false) {
		return qualityLostAck
	}
	return nil
}
func TestQualityRequestStartUnknownCommitNeverAuthorizesModel(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	ctx := context.Background()
	report, err := s.SaveReport(ctx, storeReport(prefix+"-report", storeObservation(prefix).Scope))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRequest(ctx, storeRequest(prefix, report.ID))
	if err != nil {
		t.Fatal(err)
	}
	sqldb, _ := db.DB()
	var armed atomic.Bool
	pool := &qualityLostAckPool{ConnPool: sqldb, armed: &armed}
	wrapped := db.Session(&gorm.Session{NewDB: true})
	wrapped.ConnPool = pool
	wrapped.Statement.ConnPool = pool
	uncertain, err := New(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	out, err := uncertain.StartRequest(ctx, r.ID, r.Revision, strings.Repeat("a", 64))
	if !errors.Is(err, ErrUnconfirmed) || !errors.Is(err, qualityLostAck) || out.ID != "" {
		t.Fatalf("unknown commit authorized call: %+v %v", out, err)
	}
	read, err := s.Request(ctx, r.ID)
	if err != nil || read.Status != Running || read.Revision != 2 {
		t.Fatalf("lost ack did not persist: %+v %v", read, err)
	}
	if _, err := s.StartRequest(ctx, r.ID, r.Revision, strings.Repeat("a", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed model reservation: %v", err)
	}
	if _, err := s.FinishRequest(ctx, r.ID, read.Revision, Interrupted, `{"host_stopped":true}`); err != nil {
		t.Fatal(err)
	}
}
func TestQualityMySQLDetectsPayloadAndIndexedMetadataCorruption(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	ctx := context.Background()
	o, err := s.Record(ctx, storeObservation(prefix))
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Scope: o.Scope, From: o.CreatedAt.Add(-time.Second), Until: o.CreatedAt.Add(time.Second)}
	if err := db.Model(&models.RuleObservation{}).Where("id = ?", o.ID).Update("payload_json", gorm.Expr("CONCAT(payload_json, ' ')")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, q); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("payload corruption: %v", err)
	}
	o2 := storeObservation(prefix + "-metadata")
	o2.Scope.Site += "-separate"
	saved, err := s.Record(ctx, o2)
	if err != nil {
		t.Fatal(err)
	}
	q.Scope = saved.Scope
	q.From = saved.CreatedAt.Add(-time.Second)
	q.Until = saved.CreatedAt.Add(time.Second)
	if err := db.Model(&models.RuleObservation{}).Where("id = ?", saved.ID).Update("created_at", saved.CreatedAt.Add(time.Microsecond)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, q); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("timestamp corruption: %v", err)
	}
	r, err := s.SaveReport(ctx, storeReport(prefix+"-report", storeObservation(prefix+"-report").Scope))
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.CreateRequest(ctx, storeRequest(prefix, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.RuleQualityReport{}).Where("id = ?", r.ID).Update("cutoff", r.Cutoff+1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(ctx, r.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("report indexed cutoff: %v", err)
	}
	if err := db.Model(&models.RuleReanalysisRequest{}).Where("id = ?", req.ID).Update("revision", req.Revision+1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Request(ctx, req.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("request revision corruption: %v", err)
	}
}
func TestQualityMySQLQueryKeepsConsistentCutoffDuringPrune(t *testing.T) {
	s, db, prefix := qualityMySQL(t)
	ctx := context.Background()
	o, err := s.Record(ctx, storeObservation(prefix))
	if err != nil {
		t.Fatal(err)
	}
	second := storeObservation(prefix + "-2")
	second.Scope = o.Scope
	if _, err := s.Record(ctx, second); err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	name := "quality_prune_between_reads"
	if err := db.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "crawler_rule_observations" && once.CompareAndSwap(false, true) {
			if err := db.Where("id = ?", o.ID).Delete(&models.RuleObservation{}).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	w, err := s.Query(ctx, Query{Scope: o.Scope, From: o.CreatedAt.Add(-time.Second), Until: o.CreatedAt.Add(time.Hour), Limit: 10})
	db.Callback().Query().Remove(name)
	if err != nil || len(w.Observations) != 2 || w.Cutoff < w.Observations[1].Sequence {
		t.Fatalf("snapshot changed between max/read: %+v %v", w, err)
	}
}
