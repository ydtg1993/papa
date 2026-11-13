package engine

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"strings"
	"sync"
	"testing"
)

type stubConnPool struct {
	mu    sync.Mutex
	stmts []string
}

func (p *stubConnPool) record(q string, args []any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stmts = append(p.stmts, q)
	if len(args) > 0 {
		// 参数单独记一份：连接池拿到的是占位符 SQL，值在 args 里（占位符形式也正是我们要断言的）
		p.stmts = append(p.stmts, fmt.Sprint(args...))
	}
}

func (p *stubConnPool) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, sql.ErrConnDone
}

func (p *stubConnPool) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	p.record(q, args)
	return stubResult{}, nil
}

// QueryContext 不返回 *sql.Rows，只回错误 —— 调用方走的是 error 分支，不会解引用 nil。
func (p *stubConnPool) QueryContext(_ context.Context, q string, args ...any) (*sql.Rows, error) {
	p.record(q, args)
	return nil, sql.ErrConnDone
}

func (p *stubConnPool) QueryRowContext(_ context.Context, q string, args ...any) *sql.Row {
	p.record(q, args)
	return nil
}

func (p *stubConnPool) all() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.stmts, "\n")
}

type stubResult struct{}

func (stubResult) LastInsertId() (int64, error) { return 1, nil }
func (stubResult) RowsAffected() (int64, error) { return 1, nil }

// stubFetcher 假数据：按脚本上报步骤，然后返回预设错误或 panic。
type stubFetcher struct {
	steps     []string
	err       error
	panicWith any
	seen      *Task
}

func (f *stubFetcher) GetStage() string { return "stub" }

func (f *stubFetcher) FetchHandler(_ context.Context, task *Task, _ *Engine) error {
	f.seen = task
	for _, s := range f.steps {
		// 追踪关闭时 task.Trace 为 nil —— 这里同时也是 nil 安全性的实测
		task.Trace.Step(s, map[string]string{"step": s})
	}
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	return f.err
}

// traceEngine 造一个用假连接池的引擎，不发一条真 SQL。
func traceEngine(t *testing.T, enabled bool) (*Engine, *stubConnPool) {
	t.Helper()
	pool := &stubConnPool{}
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      pool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open stub db: %v", err)
	}
	e := &Engine{
		db:        db,
		loggerSet: &loggers.LoggerSet{DB: logrus.New(), Engine: logrus.New()},
		cfg:       &config.Config{},
	}
	e.cfg.Crawler.Trace.Enabled = enabled
	e.cfg.Crawler.TraceFiles.Dir = t.TempDir()
	return e, pool
}
