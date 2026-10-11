// Package gormfake 是**给测试用的假库**：按配置返回固定的行 / 影响行数 / 错误，不做 SQL 解析
// （除了"数数"与"取行"这两种形态，见 QueryContext 的注释）。
//
// 为什么要有它：本仓的 engine 里有一套更细的假库（fakedb_test.go），但它按 `crawler_tasks`
// 的列与 WHERE 过滤写死，只服务引擎；而 admin 那几个包（auth / tokenadmin）的"查库出错就拒绝"
// 这类分支根本没法用 DryRun 测（DryRun 只拼 SQL、不返回行）。这个包补的就是这一层：
// **能返回行、能返回影响行数、能让任意查询报错**，于是 fail-closed 那几条分支也进得了单测。
//
// 放在 internal/ 是刻意的：它只给本仓的测试用，不该被业务代码 import（那会把它变成对外 API）。
package gormfake

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// New 造一个假库：**默认影响 1 行**（INSERT/UPDATE 命中的常态 —— gorm 在 RowsAffected==0 时
// 会跳过"把自增主键读回来"那段，默认给 0 会让所有 Create 都拿不到 ID，白踩一次）。
// 要造"条件没匹配上"用 SetAffected(0)。
func New() *DB { return &DB{affected: 1, lastInsertID: 1} }

// DB 一个假库。**用 New() 造**（默认影响 1 行）；零值也行，但那样影响 0 行、自增 ID 也是 0。
//
// 改它用 Set* 方法（都带锁，测试里可以并发调）。
type DB struct {
	mu sync.Mutex

	row          map[string]driver.Value // 单行查询（如 `SELECT operator … LIMIT 1`）返回的行；nil = 一行都没有
	countEnabled int64                   // `count(*)` 且 SQL 里带 enabled 条件时的返回值
	countAll     int64                   // 其余 `count(*)` 的返回值
	lastInsertID int64
	affected     int64

	// queryErr / queryErrSub：SQL 里含 queryErrSub 的查询报 queryErr（空串 = 全部查询都报）——
	// 这样能精确驱动"数数失败"与"取令牌那一句失败"这两条不同的 fail-closed 分支。
	queryErr    error
	queryErrSub string
	execErr     error // 非空：所有写操作都报这个错

	written []string // 记下所有执行过的语句（含参数），方便断言"到底写了什么"
}

// SetRow 让单行查询返回这一行（列名 → 值）。传 nil = 一行都不返回（gorm 会得到 ErrRecordNotFound）。
func (d *DB) SetRow(row map[string]driver.Value) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.row = row
}

// SetCounts 设定两种 count 的返回值：带 enabled 条件的那条（CountEnabled）与其余那条（CountAll）。
func (d *DB) SetCounts(enabled, all int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.countEnabled, d.countAll = enabled, all
}

// SetAffected 设定 INSERT/UPDATE/DELETE 的影响行数（0 = 条件没匹配上，用来驱动"409/已被改过"那类分支）。
func (d *DB) SetAffected(n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.affected = n
}

// SetLastInsertID 设定 INSERT 的 LastInsertId（gorm 写完要把自增主键读回来时用）。
func (d *DB) SetLastInsertID(id int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastInsertID = id
}

// SetQueryError 让 SQL 里含 sub 的查询报错（sub 为空串 = 所有查询都报错）。err 传空串 = 恢复不报错。
func (d *DB) SetQueryError(sub, err string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queryErrSub, d.queryErr = sub, errOf(err)
}

// SetExecError 让所有写操作报错（空串 = 不报错）。
func (d *DB) SetExecError(err string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execErr = errOf(err)
}

// Written 返回执行过的语句（含参数，空格归一）。
func (d *DB) Written() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.written, "\n")
}

func errOf(msg string) error {
	if msg == "" {
		return nil
	}
	return errors.New(msg)
}

// Open 把一个假库接成 *gorm.DB（不连真库、不需要 DSN）。
func Open(d *DB) (*gorm.DB, error) {
	if d == nil {
		d = &DB{}
	}
	// 走 sql.OpenDB(connector)：gorm 的 Config.Conn 要的是 gorm.ConnPool（*sql.DB 就满足），
	// 而我们手里只有 driver.Conn —— 与 engine 那套假库同一个接法。
	sqlDB := sql.OpenDB(connector{d: d})
	return gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true, // 版本探测要真连库，关掉
	}), &gorm.Config{
		DisableAutomaticPing: true,
		// 必须关默认事务：gorm 默认给每次写包一层 Begin/Commit，而假库不支持事务（见 conn.Begin）。
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	})
}

// MustOpen 同上，出错直接 panic（测试里）。
func MustOpen(d *DB) *gorm.DB {
	db, err := Open(d)
	if err != nil {
		panic(fmt.Sprintf("gormfake: open: %s", err))
	}
	return db
}

/* ---------- 下面是把假库接进 database/sql 的那点胶水 ---------- */

// connector 把假库接进 sql.DB。
type connector struct{ d *DB }

func (c connector) Connect(context.Context) (driver.Conn, error) { return &conn{d: c.d}, nil }
func (c connector) Driver() driver.Driver                        { return driverStub{} }

type driverStub struct{}

func (driverStub) Open(string) (driver.Conn, error) { return nil, driver.ErrSkip }

type conn struct{ d *DB }

var (
	_ driver.Conn           = (*conn)(nil)
	_ driver.QueryerContext = (*conn)(nil)
	_ driver.ExecerContext  = (*conn)(nil)
)

func (c *conn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c *conn) Close() error                        { return nil }
func (c *conn) Begin() (driver.Tx, error)           { return nil, errors.New("gormfake: 不支持事务") }

func (c *conn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.d.record(query, args)
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if c.d.execErr != nil {
		return nil, c.d.execErr
	}
	return result{lastID: c.d.lastInsertID, affected: c.d.affected}, nil
}

// QueryContext 只认两种形态（别的一律当"数数"处理）：
//
//	① `count(*)`：把 SQL 里有没有 enabled 条件当成 CountEnabled / CountAll 的区分；
//	② 其余：返回 SetRow 配的那一行（或一行都没有）。
//
// 刻意不做 WHERE 过滤：假库要的是"把这几个分支都走一遍"，不是把 MySQL 再实现一遍。
func (c *conn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.record(query, args)
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	if c.d.queryErr != nil && strings.Contains(strings.ToLower(query), strings.ToLower(c.d.queryErrSub)) {
		return nil, c.d.queryErr
	}
	if strings.Contains(strings.ToLower(query), "count(") {
		count := c.d.countAll
		if strings.Contains(strings.ToLower(query), "enabled") {
			count = c.d.countEnabled
		}
		return &rows{cols: []string{"count(*)"}, values: [][]driver.Value{{count}}}, nil
	}
	if c.d.row == nil {
		return &rows{}, nil // 一行都没有 → gorm 给 ErrRecordNotFound
	}
	cols := make([]string, 0, len(c.d.row))
	values := make([]driver.Value, 0, len(c.d.row))
	for col, v := range c.d.row {
		cols = append(cols, col)
		values = append(values, v)
	}
	return &rows{cols: cols, values: [][]driver.Value{values}}, nil
}

func (d *DB) record(query string, args []driver.NamedValue) {
	d.mu.Lock()
	defer d.mu.Unlock()
	parts := []string{strings.Join(strings.Fields(query), " ")}
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a.Value))
	}
	d.written = append(d.written, strings.Join(parts, " | "))
}

type result struct {
	lastID   int64
	affected int64
}

func (r result) LastInsertId() (int64, error) { return r.lastID, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

type rows struct {
	cols   []string
	values [][]driver.Value
	idx    int
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.idx >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.idx])
	r.idx++
	return nil
}
