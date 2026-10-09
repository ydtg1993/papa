package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
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
	failNext int      // 让接下来的 N 次写失败（模拟批量插入撞唯一索引等）
	execs    []string // 执行过的写语句
	args     [][]driver.Value
	queries  []string

	// rows 多行结果集（测分页用）。非 nil 时取代 row：按 `id > ?` 过滤后返回全部。
	// **只实现 `id > ?` 这一个条件** —— 够验分页游标，不是通用 SQL 引擎。
	rows []fakeRow
	// maxQueries > 0 时，第 maxQueries+1 次 SELECT 直接报错。给可能死循环的用例兜底：
	// 否则测试是挂住（超时才失败），而不是干脆地报出来。
	maxQueries int
	// onQuery 在每次 SELECT 之前被调用，参数是"这是第几次查询"（从 1 起）。
	// 用来模拟"第一次查不到、写失败之后又查到了"这类跨调用的状态变化
	// —— 比如 SubmitTask 插入撞唯一索引后回查那一段，没有钩子就摆不出来。
	onQuery func(n int)
	// failQueries > 0 时，接下来的 N 次 SELECT 直接报错（模拟库抖动）。
	// 与 failNext（写失败）对称：读路径的容错也要能离线演出来。
	failQueries int
	// onExec 在每次写之前被调用，参数是"这是第几次写"（从 1 起）。
	// 给"按批删除直到删不满一批"这类多轮写入用：中途改 affected 才能让它停下来。
	onExec func(n int)

	// traceRows 是 crawler_task_trace 的结果集（nil = 一张空表）。
	// 追踪是独立一张表，而 ListTrace 要的是真行，所以假库也认它。
	traceRows []fakeRow
	// count 是 COUNT(*) 查询的返回值（默认 0）。治理队列的积压采样走它 ——
	// 那条路要的是单列结果集，和 crawler_tasks 的整行结果集不是一回事。
	count int64
}

// fakeTraceColumns 是 models.TaskTrace 的全列，顺序任意 —— gorm 按列名映射。
var fakeTraceColumns = []string{
	"id", "task_id", "attempt", "seq", "step", "status", "kind", "message", "data", "duration", "created_at",
}

// traceRow 造一条步骤记录。
func traceRow(id, taskID int64, attempt, seq int, step string, status int64, data []byte) fakeRow {
	return fakeRow{
		"id": id, "task_id": taskID, "attempt": int64(attempt), "seq": int64(seq),
		"step": step, "status": status, "kind": "", "message": "",
		"data": data, "duration": int64(3 * time.Millisecond), "created_at": time.Now(),
	}
}

// fakeRow 一行 crawler_task 的值；某列缺席即为 NULL。
type fakeRow map[string]driver.Value

// limitRe 匹配 gorm 内联渲染的 LIMIT；多行模式下据此截断结果集。
var limitRe = regexp.MustCompile(`(?i)LIMIT\s+(\d+)`)

// fakeTaskColumns 是 models.CrawlerTask 的全列，顺序任意 —— gorm 按列名映射。
var fakeTaskColumns = []string{
	"id", "pid", "stage", "url", "idempotency_key", "meta", "title", "content",
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
			"idempotency_key": "", "meta": []byte("{}"), "title": "", "content": []byte("{}"),
			"retry": int64(0), "status": int64(models.TaskStatusPending), "repeatable": int64(0),
			"repeat": int64(0), "reprocess": int64(0), "urgent": false, "error": "",
			"created_at": now, "updated_at": now,
		},
	}
}

// rowWithID 复制「一行待处理任务」并把 id 换掉，用来造多行结果集。
func (f *fakeTaskDB) rowWithID(id int64) fakeRow {
	out := make(fakeRow, len(f.row))
	for k, v := range f.row {
		out[k] = v
	}
	out["id"] = id
	return out
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

// readSQL 返回执行过的读语句（SELECT），用来断言回查条件。
func (f *fakeTaskDB) readSQL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.queries, "\n")
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
	if f.onExec != nil {
		f.onExec(len(f.execs))
	}
	if f.failNext > 0 {
		f.failNext--
		return nil, errors.New("fake: duplicate entry")
	}
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	f.args = append(f.args, vals)
	return fakeResult{affected: f.affected}, nil
}

// rowValues 把一行按 fakeTaskColumns 摊成 driver.Value 切片。
func rowValues(r fakeRow) []driver.Value { return rowValuesOf(fakeTaskColumns, r) }

// rowValuesOf 按给定列名摊平一行；缺席的列即 NULL。
func rowValuesOf(cols []string, r fakeRow) []driver.Value {
	vals := make([]driver.Value, len(cols))
	for i, c := range cols {
		vals[i] = r[c]
	}
	return vals
}

func (f *fakeTaskDB) query(q string, args []driver.NamedValue) (driver.Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.onQuery != nil {
		f.onQuery(len(f.queries))
	}
	if f.failQueries > 0 {
		f.failQueries--
		return nil, errors.New("fake: connection reset by peer")
	}

	if strings.Contains(q, "crawler_task_trace") {
		out := &fakeRows{cols: fakeTraceColumns}
		for _, r := range f.traceRows {
			out.all = append(out.all, rowValuesOf(fakeTraceColumns, r))
		}
		return out, nil
	}
	if strings.Contains(strings.ToUpper(q), "COUNT(") {
		return &fakeRows{cols: []string{"count(*)"}, all: [][]driver.Value{{f.count}}}, nil
	}
	if !strings.Contains(q, "crawler_tasks") {
		return nil, fmt.Errorf("fakeTaskDB 只认 crawler_tasks / crawler_task_trace，收到：%s", q)
	}
	if f.maxQueries > 0 && len(f.queries) > f.maxQueries {
		return nil, fmt.Errorf("fakeTaskDB: SELECT 已执行 %d 次，超过上限 %d（被测逻辑可能在死循环）",
			len(f.queries), f.maxQueries)
	}
	if f.rows == nil {
		if f.noRows {
			return &fakeRows{cols: fakeTaskColumns}, nil // 一行都没有 → gorm 回 ErrRecordNotFound
		}
		return &fakeRows{cols: fakeTaskColumns, all: [][]driver.Value{rowValues(f.row)}}, nil
	}

	// 参数按 SQL 里 `?` 的出现顺序绑定：`status IN (?,?)` → `id > ?`（若有）→ `LIMIT ?`（若有）。
	// LIMIT 在本仓库的 gorm 下是**绑定**的（`LIMIT ?`）而且总在最后一个，所以先把它摘掉，
	// 剩下的尾参数才是 keyset 游标 —— 直接取 `args[len-1]` 会拿到 limit。
	argN := len(args)
	limit := -1
	if strings.Contains(strings.ToUpper(q), "LIMIT ?") && argN > 0 {
		if v, ok := asInt64(args[argN-1].Value); ok {
			limit = int(v)
		}
		argN--
	}
	var lastID int64
	if strings.Contains(q, "id > ?") && argN > 0 {
		if v, ok := asInt64(args[argN-1].Value); ok {
			lastID = v
		}
	}
	out := &fakeRows{cols: fakeTaskColumns}
	for _, r := range f.rows {
		if id, _ := r["id"].(int64); id > lastID {
			out.all = append(out.all, rowValues(r))
		}
	}
	// 不照 LIMIT 截断的话，一批就把所有行都返回了 —— 分页根本没被走到，测试会假通过。
	// 内联写法（`LIMIT 1`）也认，免得换个 gorm 版本就静默失效。
	if limit < 0 {
		if m := limitRe.FindStringSubmatch(q); m != nil {
			limit, _ = strconv.Atoi(m[1])
		}
	}
	if limit >= 0 && limit < len(out.all) {
		out.all = out.all[:limit]
	}
	return out, nil
}

// asInt64 把绑定参数当整数取（驱动层会把 uint 归一成 int64）。
func asInt64(v driver.Value) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
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

func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.f.query(q, args)
}

type fakeResult struct{ affected int64 }

func (r fakeResult) LastInsertId() (int64, error) { return 1, nil }
func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

type fakeRows struct {
	cols []string
	all  [][]driver.Value // 空（nil）= 一行都没有
	idx  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.all) {
		return io.EOF
	}
	copy(dest, r.all[r.idx])
	r.idx++
	return nil
}
