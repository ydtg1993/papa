package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v2/config"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// upsertRecord 是 Upsert 的最小载体：一个自增主键 + 一个唯一约束列。
type upsertRecord struct {
	ID    uint   `gorm:"primarykey"`
	URL   string `gorm:"type:varchar(500);uniqueIndex:uniq_url"`
	Title string `gorm:"type:text"`
}

func (upsertRecord) TableName() string { return "upsert_records" }

var upsertColumns = []string{"id", "url", "title"}

/* ---------- 最小假驱动 ---------- */

// stubDB 记下执行过的语句，并可控地返回 LastInsertId / 结果行。
//
// 为什么需要它：Upsert 的**冲突更新路径**（ON DUPLICATE KEY UPDATE 不回填自增主键，
// 要自己按唯一键查回来）只在 LastInsertId=0 时才走到，DryRun 又拿不到结果集。
type stubDB struct {
	mu sync.Mutex

	execs   []string
	args    [][]driver.Value
	queries []string

	// queryFn 非 nil 时接管查询：按 (语句, 参数) 返回结果集。
	// 迁移那套守卫查询全靠它 —— 它们形状相同（COUNT(*) / DATA_TYPE），只有参数能区分问的是哪张表/哪个列。
	queryFn func(q string, args []driver.Value) (cols []string, rows [][]driver.Value)

	lastInsertID int64 // 0 表示"冲突更新路径"（gorm 回填不到主键）
	affected     int64
	row          []driver.Value // 回查时返回的那一行（nil = 空结果集）
	failExec     bool
	failQuery    bool
}

func newStubDB() *stubDB {
	return &stubDB{
		lastInsertID: 1,
		affected:     1,
		row:          []driver.Value{int64(9), "https://example.com", "原标题"},
	}
}

func openStubDB(t *testing.T, s *stubDB) *gorm.DB {
	t.Helper()
	sqlDB := sql.OpenDB(&stubConnector{s: s})
	t.Cleanup(func() { _ = sqlDB.Close() })

	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("open stub db: %v", err)
	}
	return db
}

func (s *stubDB) written() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.execs, "\n")
}

func (s *stubDB) writtenArgs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	for _, a := range s.args {
		for _, v := range a {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return strings.Join(parts, " ")
}

func (s *stubDB) readSQL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.queries, "\n")
}

func (s *stubDB) exec(q string, args []driver.NamedValue) (driver.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs = append(s.execs, q)
	if s.failExec {
		return nil, errors.New("stub: write failed")
	}
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	s.args = append(s.args, vals)
	return stubResult{lastID: s.lastInsertID, affected: s.affected}, nil
}

func (s *stubDB) query(q string, args []driver.Value) (driver.Rows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	if s.failQuery {
		return nil, errors.New("stub: read failed")
	}
	if s.queryFn != nil {
		cols, all := s.queryFn(q, args)
		return &stubRows{cols: cols, all: all}, nil
	}
	rows := &stubRows{cols: upsertColumns}
	if s.row != nil {
		rows.all = [][]driver.Value{s.row}
	}
	return rows, nil
}

type stubConnector struct{ s *stubDB }

func (c *stubConnector) Connect(context.Context) (driver.Conn, error) { return &stubConn{s: c.s}, nil }
func (c *stubConnector) Driver() driver.Driver                        { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("stubDB 只支持 sql.OpenDB(stubConnector)")
}

type stubConn struct{ s *stubDB }

func (c *stubConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *stubConn) Close() error                        { return nil }
func (c *stubConn) Begin() (driver.Tx, error)           { return nil, errors.New("stubDB 不支持事务") }

func (c *stubConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.s.exec(q, args)
}

func (c *stubConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	return c.s.query(q, vals)
}

type stubResult struct {
	lastID   int64
	affected int64
}

func (r stubResult) LastInsertId() (int64, error) { return r.lastID, nil }
func (r stubResult) RowsAffected() (int64, error) { return r.affected, nil }

type stubRows struct {
	cols []string
	all  [][]driver.Value
	idx  int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.all) {
		return io.EOF
	}
	copy(dest, r.all[r.idx])
	r.idx++
	return nil
}

/* ---------- columnList ---------- */

func TestColumnList(t *testing.T) {
	got := columnList([]string{"url", "stage"})
	if len(got) != 2 || got[0].Name != "url" || got[1].Name != "stage" {
		t.Fatalf("columnList = %+v", got)
	}
	if got := columnList(nil); len(got) != 0 {
		t.Fatalf("空输入应得到空切片：%+v", got)
	}
}

/* ---------- Upsert ---------- */

// 插入路径：一条 INSERT ... ON DUPLICATE KEY UPDATE，主键由驱动回填，不必再查。
func TestUpsertInsertPath(t *testing.T) {
	s := newStubDB()
	db := openStubDB(t, s)

	rec := &upsertRecord{URL: "https://example.com", Title: "新标题"}
	if err := Upsert(db, rec, []string{"url"}, []string{"title"}); err != nil {
		t.Fatalf("Upsert = %v", err)
	}

	sql := s.written()
	if !strings.Contains(sql, "INSERT INTO `upsert_records`") {
		t.Fatalf("应插 upsert_records：\n%s", sql)
	}
	if !strings.Contains(sql, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("应带上冲突更新子句：\n%s", sql)
	}
	if rec.ID != 1 {
		t.Fatalf("主键应被回填（驱动给的是 1），实得 %d", rec.ID)
	}
	if s.readSQL() != "" {
		t.Fatalf("插入路径不该回查：\n%s", s.readSQL())
	}
}

// 冲突更新路径：ON DUPLICATE KEY UPDATE 不回填自增主键，
// 所以必须按唯一键把那一行**查回来**并回填到 record —— 否则调用方拿到的是 ID=0，
// 后续拿它去 SaveResult / 关联子任务全会指向一条不存在的行。
func TestUpsertConflictPathBackfillsFromQuery(t *testing.T) {
	s := newStubDB()
	s.lastInsertID = 0 // gorm 回填不到主键 → 走回查
	db := openStubDB(t, s)

	rec := &upsertRecord{URL: "https://example.com", Title: "新标题"}
	if err := Upsert(db, rec, []string{"url"}, []string{"title"}); err != nil {
		t.Fatalf("Upsert = %v", err)
	}

	if rec.ID != 9 {
		t.Fatalf("应回填库里那一行的 ID（假驱动给的是 9），实得 %d", rec.ID)
	}
	// 回查条件必须按 conflictColumns 来
	read := s.readSQL()
	if !strings.Contains(read, "`url` = ?") {
		t.Fatalf("回查应带唯一键条件：\n%s", read)
	}
	if !strings.Contains(s.writtenArgs(), "https://example.com") {
		t.Fatalf("回查要用 record 上的值：%s", s.writtenArgs())
	}
}

// 回查查不到（理论上不该发生）：把错误透出来，而不是留一个 ID=0 的 record 在调用方手里。
func TestUpsertConflictPathReportsMissingRow(t *testing.T) {
	s := newStubDB()
	s.lastInsertID = 0
	s.row = nil // 空结果集
	db := openStubDB(t, s)

	rec := &upsertRecord{URL: "https://example.com"}
	err := Upsert(db, rec, []string{"url"}, []string{"title"})
	if err == nil {
		t.Fatal("回查不到应当报错")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("应透出 gorm 的哨兵错误，实得 %v", err)
	}
}

// 写库失败原样返回，不去回查。
func TestUpsertReturnsWriteError(t *testing.T) {
	s := newStubDB()
	s.failExec = true
	db := openStubDB(t, s)

	err := Upsert(db, &upsertRecord{URL: "https://example.com"}, []string{"url"}, []string{"title"})
	if err == nil {
		t.Fatal("写失败应当报错")
	}
	if s.readSQL() != "" {
		t.Fatalf("写失败后不该回查：\n%s", s.readSQL())
	}
}

// 冲突列写错了（不在模型上）要**明确报出来**：静默跳过的话 Upsert 会退化成
// "每次都插入一行新的"，而唯一索引又会把它顶回来 —— 调用方完全看不出问题在哪。
func TestUpsertRejectsUnknownConflictColumn(t *testing.T) {
	s := newStubDB()
	s.lastInsertID = 0
	db := openStubDB(t, s)

	err := Upsert(db, &upsertRecord{URL: "https://example.com"}, []string{"nope"}, []string{"title"})
	if err == nil {
		t.Fatal("未知的冲突列应当报错")
	}
	if !strings.Contains(err.Error(), "conflict column") {
		t.Fatalf("错误信息 = %v", err)
	}
}

// record 不是非 nil 指针时给一句人话，而不是 reflect 层的 panic。
func TestUpsertRejectsNonPointerRecord(t *testing.T) {
	s := newStubDB()
	s.lastInsertID = 0
	db := openStubDB(t, s)

	err := Upsert(db, upsertRecord{URL: "x"}, []string{"url"}, []string{"title"})
	if err == nil {
		t.Fatal("非指针 record 应当报错")
	}
}

/* ---------- NewDB ---------- */

// 驱动名不认识时在建连之前就拒掉，并说清是哪个驱动。
func TestNewDBUnsupportedDriver(t *testing.T) {
	_, err := NewDB(&config.Config{DB: config.DBConfig{Driver: "postgres", DSN: "x"}})
	if err == nil {
		t.Fatal("不支持的驱动应当报错")
	}
	if !strings.Contains(err.Error(), "unsupported driver") || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("错误信息 = %v", err)
	}
}

// DSN 连不上时报的是"连不上"，不是别的。
func TestNewDBConnectFailure(t *testing.T) {
	// 127.0.0.1:1 会立刻拒绝
	_, err := NewDB(&config.Config{DB: config.DBConfig{
		Driver: "mysql", DSN: "u:p@tcp(127.0.0.1:1)/x?timeout=500ms",
	}})
	if err == nil {
		t.Fatal("连不上应当报错")
	}
	if !strings.Contains(err.Error(), "failed to connect database") {
		t.Fatalf("错误信息 = %v", err)
	}
}

/* ---------- AutoMigrate ---------- */

// 一个模型都没有时直接返回 nil，一条语句都不发。
func TestAutoMigrateNoModels(t *testing.T) {
	s := newStubDB()
	if err := AutoMigrate(openStubDB(t, s)); err != nil {
		t.Fatalf("AutoMigrate() = %v", err)
	}
	if s.written() != "" || s.readSQL() != "" {
		t.Fatalf("没有模型时不该碰库：exec=%q query=%q", s.written(), s.readSQL())
	}
}

// 迁移出错要停下并原样返回 —— 静默跳过会让"表没建"变成运行时才暴露的问题。
//
// 要让它真的走到 DDL：信息架构查询必须失败（否则 gorm 认为表已存在、什么都不做），
// 然后建表语句再失败 —— 这一条错误是 AutoMigrate 里**显式返回**的，不依赖 gorm 内部的 Error 传递。
func TestAutoMigrateStopsOnError(t *testing.T) {
	s := newStubDB()
	s.failQuery = true
	s.failExec = true
	err := AutoMigrate(openStubDB(t, s), &upsertRecord{}, &upsertRecord{})
	if err == nil {
		t.Fatal("迁移失败应当报错")
	}
}
