package crawler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// fakeTaskDB 假 MySQL：库里只放一行 crawler_task，够用来跑「先查一行、再条件更新」这类路径。
//
// 为什么不复用 trace_test.go 里的 stubConnPool：那个只记语句、QueryContext 一律回错误，
// 拿不到 *sql.Rows。而 UrgentTask 得先把行读出来才知道 URL/Stage/状态，没有结果集就走不下去。
// 假 driver 也不依赖 gorm 的 DryRun —— 实测本仓库的 gorm 版本下 DryRun 拦不住 Create/Update。
type fakeTaskDB struct {
	mu sync.Mutex

	row      fakeRow  // 当前那一行（按列名）；配合 noRows 决定是"有一行"还是"查不到"
	noRows   bool     // true 时 SELECT 返回空结果集（模拟行不存在）
	affected int64    // UPDATE 的 RowsAffected，用来模拟条件没匹配上
	execs    []string // 执行过的写语句
	args     [][]driver.Value
	queries  []string
}

// fakeRow 一行 crawler_task 的值；某列缺席即为 NULL。
type fakeRow map[string]driver.Value

// fakeTaskColumns 是 models.CrawlerTask 的全列，顺序任意 —— gorm 按列名映射。
var fakeTaskColumns = []string{
	"id", "pid", "stage", "url", "idempotency_key", "title", "content",
	"retry", "status", "repeatable", "repeat", "reprocess", "urgent", "error",
	"created_at", "updated_at",
}

// newFakeTaskDB 造一个放着「一行待处理、未加急任务」的假库。
func newFakeTaskDB() *fakeTaskDB {
	now := time.Now()
	return &fakeTaskDB{
		affected: 1,
		row: fakeRow{
			"id": int64(7), "pid": int64(0), "stage": "stub", "url": "https://example.com",
			"idempotency_key": "", "title": "", "content": []byte("{}"),
			"retry": int64(0), "status": int64(models.TaskStatusPending), "repeatable": int64(0),
			"repeat": int64(0), "reprocess": int64(0), "urgent": false, "error": "",
			"created_at": now, "updated_at": now,
		},
	}
}

// openFakeTaskDB 把假库接进 gorm。SkipDefaultTransaction 是为了不让 gorm 包事务
// （假 driver 不支持 Begin），对被测逻辑没有影响。
func openFakeTaskDB(t *testing.T, f *fakeTaskDB) *gorm.DB {
	t.Helper()
	sqlDB := sql.OpenDB(&fakeConnector{f: f})
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
		t.Fatalf("open fake db: %v", err)
	}
	return db
}

// writtenArgs 把已执行语句的参数摊成字符串 —— 假 driver 走的是占位符 SQL，
// 值在 args 里（这也是我们要断言的形态：守卫写在语句里，不是拿到 Go 里判）。
func (f *fakeTaskDB) writtenArgs() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for _, a := range f.args {
		for _, v := range a {
			parts = append(parts, fmt.Sprint(v))
		}
	}
	return strings.Join(parts, " ")
}

// hasArg 判断某个绑定参数里出现过 s（测试里用来确认追加内容真的传进了 SQL）。
func (f *fakeTaskDB) writtenArgs_hasError(s string) bool {
	return strings.Contains(f.writtenArgs(), s)
}

func (f *fakeTaskDB) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.execs, "\n")
}

func (f *fakeTaskDB) exec(q string, args []driver.NamedValue) (driver.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, q)
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	f.args = append(f.args, vals)
	return fakeResult{affected: f.affected}, nil
}

func (f *fakeTaskDB) query(q string) (driver.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)

	if !strings.Contains(q, "crawler_tasks") {
		return nil, fmt.Errorf("fakeTaskDB 只认 crawler_tasks，收到：%s", q)
	}
	if f.noRows {
		return &fakeRows{cols: fakeTaskColumns}, nil // 一行都没有 → gorm 回 ErrRecordNotFound
	}
	vals := make([]driver.Value, len(fakeTaskColumns))
	for i, c := range fakeTaskColumns {
		vals[i] = f.row[c]
	}
	return &fakeRows{cols: fakeTaskColumns, vals: vals}, nil
}

/* ---------- 最小 database/sql/driver 实现 ---------- */

type fakeConnector struct{ f *fakeTaskDB }

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{f: c.f}, nil }
func (c *fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakeTaskDB 只支持 sql.OpenDB(fakeConnector)")
}

type fakeConn struct{ f *fakeTaskDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("fakeTaskDB 不支持事务") }

func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return c.f.exec(q, args)
}

func (c *fakeConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	return c.f.query(q)
}

type fakeResult struct{ affected int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

type fakeRows struct {
	cols []string
	vals []driver.Value // nil 表示"一行都没有"
	sent bool
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.vals == nil || r.sent {
		return io.EOF
	}
	r.sent = true
	copy(dest, r.vals)
	return nil
}
