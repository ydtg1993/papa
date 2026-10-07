package oplog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

/* ---------- 假库 + 记日志的 logger ---------- */

// stubPool 假连接池：只实现 Create 会用到的那一条，由测试控制成功 / 失败 / 卡住。
// 不用 gorm 的 DryRun —— 实测本仓库的 gorm 版本下 DryRun 拦不住 Create，照样会去连库。
type stubPool struct {
	mu      sync.Mutex
	execs   int
	execErr error
	block   chan struct{} // 非 nil 时 ExecContext 会等它关闭，用来模拟"写库跟不上"
}

func (p *stubPool) PrepareContext(context.Context, string) (*sql.Stmt, error) {
	return nil, sql.ErrConnDone
}

func (p *stubPool) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, sql.ErrConnDone
}

func (p *stubPool) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func (p *stubPool) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.execs++
	if p.execErr != nil {
		return nil, p.execErr
	}
	return stubResult{}, nil
}

func (p *stubPool) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.execs
}

type stubResult struct{}

func (stubResult) LastInsertId() (int64, error) { return 1, nil }
func (stubResult) RowsAffected() (int64, error) { return 1, nil }

func openStubDB(t *testing.T, p *stubPool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      p,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatalf("open stub db: %v", err)
	}
	return db
}

// memLogger 收下所有落到日志文件的行 —— 它就是审计的"备选落地"。
type memLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *memLogger) Errorf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *memLogger) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

/* ---------- 测试 ---------- */

// 入队即可返回，落库由消费协程做；Close 要把队列里剩下的写完。
func TestRecorderWritesEveryRecord(t *testing.T) {
	p := &stubPool{}
	rec := New(openStubDB(t, p), &memLogger{})

	const n = 50
	for i := 0; i < n; i++ {
		rec.RecordEvent(Event{Table: "task", Action: "retry", RowID: fmt.Sprint(i), At: time.Now()})
	}
	rec.Close(2 * time.Second)

	if got := p.count(); got != n {
		t.Fatalf("应写 %d 条，实得 %d（Close 没排空？）", n, got)
	}
}

// 库写不进去时，审计必须落到日志文件，而且是完整可捞回来补录的 JSON。
func TestRecorderFallsBackToLogFileOnDBError(t *testing.T) {
	p := &stubPool{execErr: errors.New("db is down")}
	lg := &memLogger{}
	rec := New(openStubDB(t, p), lg)

	rec.RecordEvent(Event{Table: "task", Action: "retry", RowID: "7", Operator: "张三",
		IP: "10.0.0.1", At: time.Now()})
	rec.Close(2 * time.Second)

	got := lg.joined()
	if !strings.Contains(got, "已落到日志文件") {
		t.Fatalf("应在日志里说明落到了文件：%s", got)
	}
	for _, want := range []string{`"table":"task"`, `"action":"retry"`, `"row_id":"7"`, `"operator":"张三"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("落到日志的那份应当是完整 JSON，缺少 %s：%s", want, got)
		}
	}
}

// 队列满（写库跟不上）时也不能反压业务响应，只能降到日志文件。
func TestRecorderDegradesToLogWhenQueueFull(t *testing.T) {
	p := &stubPool{block: make(chan struct{})} // 消费协程卡在写库里 → 队列很快满
	lg := &memLogger{}
	rec := New(openStubDB(t, p), lg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < oplogQueueSize*2; i++ {
			rec.RecordEvent(Event{Table: "task", Action: "retry", RowID: fmt.Sprint(i), At: time.Now()})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("入队被阻塞了 —— 审计绝不能反压业务响应")
	}

	close(p.block) // 放消费协程走完，好让 Close 能排空
	rec.Close(3 * time.Second)

	if !strings.Contains(lg.joined(), "队列已满") {
		t.Fatalf("队列满时应降到日志文件：%s", lg.joined())
	}
}

// 关停之后再来记录：降到日志文件，而不是 panic（向已关闭的 channel 发送）。
func TestRecorderAfterCloseFallsBackInsteadOfPanic(t *testing.T) {
	p := &stubPool{}
	lg := &memLogger{}
	rec := New(openStubDB(t, p), lg)

	rec.Close(time.Second)
	rec.Close(time.Second) // 幂等
	rec.RecordEvent(Event{Table: "task", Action: "retry", RowID: "7", At: time.Now()})

	if p.count() != 0 {
		t.Fatalf("关停后不该再写库，实得 %d", p.count())
	}
	if !strings.Contains(lg.joined(), "已停止") {
		t.Fatalf("关停后的记录应降到日志文件：%s", lg.joined())
	}
}
