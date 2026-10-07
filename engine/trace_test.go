package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/models"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// 追踪未开启（Task.Trace 为 nil）时，handler 里的调用必须是安全的 no-op ——
// 这是「可选协作者真的可选」这条承诺的全部内容。
func TestTraceNilReceiverIsNoop(t *testing.T) {
	var tr *Trace
	tr.Step("解析列表", map[string]string{"n": "1"})
	tr.Fail("解析详情", errors.New("boom"), nil)
	tr.setResult(nil)
	tr.finish()
	if got := tr.flush(); got != nil {
		t.Fatalf("nil trace flush = %+v, want nil", got)
	}
}

func TestTraceBuffersStepsWithSeqAndDuration(t *testing.T) {
	tr := newTrace(dryDB(t), logrus.New(), 7, 2)
	tr.Step("打开页面", nil)
	time.Sleep(2 * time.Millisecond)
	tr.Step("解析列表", nil)

	records := tr.flush()
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	for i, r := range records {
		if r.TaskID != 7 || r.Attempt != 2 {
			t.Fatalf("record %d 关联错了：task=%d attempt=%d", i, r.TaskID, r.Attempt)
		}
		if r.Seq != i {
			t.Fatalf("record %d seq = %d, want %d", i, r.Seq, i)
		}
	}
	// 耗时是「距上一个 Step」的间隔，所以上一步一定不小于我们注入的那段 sleep
	if records[1].Duration < 2*time.Millisecond {
		t.Fatalf("第二步耗时 = %v, 应覆盖中间的 sleep", records[1].Duration)
	}
}

// flush 幂等：正常路径调一次、defer 兜底再调一次，不能重复写。
func TestTraceFlushIsIdempotent(t *testing.T) {
	tr := newTrace(dryDB(t), logrus.New(), 7, 0)
	tr.Step("第一步", nil)
	if got := tr.flush(); len(got) != 1 {
		t.Fatalf("首次 flush = %d 条, want 1", len(got))
	}
	if got := tr.flush(); got != nil {
		t.Fatalf("二次 flush = %+v, want nil（幂等）", got)
	}
}

// 成功尝试不写 data（绝大多数任务走这条路，写入量按失败率走）；
// 失败尝试保留 data —— panic 时 setResult 根本没被调用，也落在这一侧。
func TestTraceDataOnlyKeptForFailedAttempts(t *testing.T) {
	cases := []struct {
		name      string
		setResult func(*Trace)
		wantData  bool
	}{
		{"成功尝试剥离 data", func(tr *Trace) { tr.setResult(nil) }, false},
		{"失败尝试保留 data", func(tr *Trace) { tr.setResult(errors.New("boom")) }, true},
		{"没走到 setResult（panic）按失败处理", func(tr *Trace) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTrace(dryDB(t), logrus.New(), 7, 0)
			tr.Step("采集列表", map[string]string{"count": "3"})
			tr.Fail("解析详情", errors.New("selector not found"), map[string]string{"url": "u"})
			tc.setResult(tr)

			records := tr.flush()
			if len(records) != 2 {
				t.Fatalf("records = %d, want 2", len(records))
			}
			for i, r := range records {
				if tc.wantData && len(r.Data) == 0 {
					t.Fatalf("第 %d 步的 data 不应被剥离：%s", i, r.Data)
				}
				if !tc.wantData && len(r.Data) != 0 {
					t.Fatalf("第 %d 步的 data 应被剥离，实得 %s", i, r.Data)
				}
			}
			// 失败步的分类与信息，无论 data 保不保留都要在
			if records[1].Status != models.TraceFailed || records[1].Kind == "" || records[1].Message == "" {
				t.Fatalf("失败步缺分类/信息：%+v", records[1])
			}
		})
	}
}

// 一次尝试 = 一条多行 INSERT，不是每步一条。用 DryRun 的 DB 把语句抓出来看。
func TestTraceWritesOneMultiRowInsert(t *testing.T) {
	db := dryDB(t)
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		tr := newTrace(tx, logrus.New(), 7, 0)
		tr.Step("第一步", map[string]string{"k": "v"})
		tr.Fail("第二步", errors.New("boom"), nil)
		tr.setResult(errors.New("boom"))
		tr.finish()
		return tx
	})

	if n := strings.Count(sql, "INSERT INTO"); n != 1 {
		t.Fatalf("应有且仅有一条 INSERT，实得 %d 条：\n%s", n, sql)
	}
	if !strings.Contains(sql, "crawler_task_trace") {
		t.Fatalf("写错表了：\n%s", sql)
	}
	for _, want := range []string{"第一步", "第二步", "boom"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("INSERT 里缺少 %q：\n%s", want, sql)
		}
	}
}

// 序列化不了的数据不静默丢：写一条可读的标记，免得排查时以为 handler 没上报。
func TestTraceUnmarshalableDataLeavesMarker(t *testing.T) {
	tr := newTrace(dryDB(t), logrus.New(), 7, 0)
	tr.Fail("采集", errors.New("boom"), make(chan int))
	records := tr.flush()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if !strings.Contains(string(records[0].Data), "_marshal_error") {
		t.Fatalf("data = %s, want 留下 _marshal_error 标记", records[0].Data)
	}
}

// 引擎闭包里的 defer 兜底依赖这条性质：panic 展开时 defer 照常执行，
// 所以 panic 之前已经上报的步骤不会丢。（handler panic 本身仍由 workerpool 的 recover
// 兜住、栈照旧进 Errors() —— 那边有 TestHandlerPanicKeepsWorker 钉着，这里只钉我们这一层。）
func TestTraceFlushSurvivesPanicUnwind(t *testing.T) {
	var flushed []models.TaskTrace
	func() {
		defer func() { _ = recover() }()
		tr := newTrace(dryDB(t), logrus.New(), 7, 0)
		defer func() { flushed = tr.flush() }() // 引擎闭包里那个 defer
		tr.Step("打开页面", map[string]string{"url": "u"})
		tr.Step("解析列表", map[string]string{"count": "3"})
		panic("handler 炸了")
	}()

	if len(flushed) != 2 {
		t.Fatalf("panic 后 flush 出 %d 条, want 2（panic 前的步骤都该在）", len(flushed))
	}
	for i, r := range flushed {
		if len(r.Data) == 0 {
			t.Fatalf("第 %d 步的 data 不该被剥离（没走到 setResult，按失败处理）", i)
		}
	}
}

func TestNewTraceRespectsSwitch(t *testing.T) {
	e := &Engine{cfg: &config.Config{}, loggerSet: &loggers.LoggerSet{DB: logrus.New()}}
	e.cfg.Crawler.Trace.Enabled = false
	if got := e.newTrace(&Task{ID: 1}, 0); got != nil {
		t.Fatal("追踪关闭时不应造记录器")
	}

	e.cfg.Crawler.Trace.Enabled = true
	if got := e.newTrace(&Task{ID: 0}, 0); got != nil {
		t.Fatal("任务还没落库（没有行可挂）时不应造记录器")
	}
	if got := e.newTrace(&Task{ID: 1}, 0); got == nil {
		t.Fatal("开启且已落库时应造记录器")
	}
}

func TestTraceRetentionDefaults(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	if got := e.traceRetention(); got != defaultTraceRetention {
		t.Fatalf("未配置保留期 = %v, want %v", got, defaultTraceRetention)
	}
	e.cfg.Crawler.Trace.Retention = -time.Hour
	if got := e.traceRetention(); got != 0 {
		t.Fatalf("负数保留期 = %v, want 0（不自动清理）", got)
	}
	e.cfg.Crawler.Trace.Retention = 48 * time.Hour
	if got := e.traceRetention(); got != 48*time.Hour {
		t.Fatalf("显式保留期 = %v, want 48h", got)
	}
}

/* ---------- 引擎侧：用假连接池 + 假 fetcher 跑通整条尝试路径（不连库、不起浏览器） ---------- */

// stubConnPool 假连接池：不连库，只把 GORM 生成的语句记下来，并当作成功返回。
//
// 这里不用 gorm 的 DryRun：实测本仓库的 gorm 版本（v1.31.1）下，
// `Config{DryRun: true}` 与 `Session{DryRun: true}` 都拦不住 Create/Update，照样去连库
// （gormsource 的测试没暴露这一点，因为它只断言 SQL、不看错误）。
// 直接换连接池是确定的，也不依赖 gorm 的内部开关。
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

// traceDataColumn 是 data 列在 INSERT 里的两种形态：
// 成功尝试被剥离成字面 NULL，失败尝试是绑定的参数（gorm 对 JSON 列会包一层 CAST）。
// 用它们断言 data 的取舍，比去翻参数更直接。
const traceDataColumn = "CAST(? AS JSON)"

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
	return e, pool
}

func TestRunAttemptFlushesTrace(t *testing.T) {
	e, pool := traceEngine(t, true)
	f := &stubFetcher{steps: []string{"打开列表页", "解析字段"}}
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}

	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if f.seen.Trace == nil {
		t.Fatal("追踪开启时 handler 里应拿到非 nil 的 recorder")
	}

	got := pool.all()
	if n := strings.Count(got, "INSERT INTO"); n != 1 {
		t.Fatalf("一次尝试应只有一条 INSERT，实得 %d：\n%s", n, got)
	}
	for _, want := range []string{"crawler_task_trace", "打开列表页", "解析字段"} {
		if !strings.Contains(got, want) {
			t.Fatalf("SQL 里缺少 %q：\n%s", want, got)
		}
	}
	// 成功的尝试不写 data
	if strings.Contains(got, traceDataColumn) {
		t.Fatalf("成功的尝试不该写 data：\n%s", got)
	}
}

func TestRunAttemptWithoutTraceWritesNothing(t *testing.T) {
	e, pool := traceEngine(t, false)
	f := &stubFetcher{steps: []string{"打开列表页"}}
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}

	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if f.seen.Trace != nil {
		t.Fatal("追踪关闭时 handler 里应是 nil（调用是 no-op）")
	}
	if got := pool.all(); strings.Contains(got, "crawler_task_trace") {
		t.Fatalf("追踪关闭时不该写 trace：\n%s", got)
	}
}

// panic 必须照旧抛给 workerpool（它那边的栈诊断与 failed 计数不能被动），
// 同时 panic 之前上报的步骤要已经落库。
func TestRunAttemptFlushesTraceOnPanic(t *testing.T) {
	e, pool := traceEngine(t, true)
	f := &stubFetcher{steps: []string{"打开列表页", "解析字段"}, panicWith: "handler 炸了"}
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("panic 必须照旧抛出，不能被追踪层吞掉")
		}
		got := pool.all()
		for _, want := range []string{"打开列表页", "解析字段"} {
			if !strings.Contains(got, want) {
				t.Fatalf("panic 之前上报的步骤 %q 未落库：\n%s", want, got)
			}
		}
		// panic 路径上 setResult 没被调用 → 按失败处理 → data 保留
		if !strings.Contains(got, traceDataColumn) {
			t.Fatalf("panic 的尝试应保留 data：\n%s", got)
		}
	}()

	_ = e.runAttempt(context.Background(), f, task, 0)
}

// 加急任务在 trace 里留一步 —— claimTask 认领时会把 urgent 列归零，
// 不在 trace 里记的话，事后就再也看不出这条曾经加急跑过。
func TestRunAttemptRecordsUrgentMarker(t *testing.T) {
	e, pool := traceEngine(t, true)
	f := &stubFetcher{steps: []string{"打开列表页"}}
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com", Urgent: true}

	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	got := pool.all()
	if !strings.Contains(got, traceUrgentStep) {
		t.Fatalf("加急任务应在 trace 里记一步 %q：\n%s", traceUrgentStep, got)
	}
	// 记在第一步：时间线上排在 handler 自己的步骤之前
	if i, j := strings.Index(got, traceUrgentStep), strings.Index(got, "打开列表页"); i > j {
		t.Fatalf("加急标记应排在 handler 的步骤之前：\n%s", got)
	}
}

func TestRunAttemptNoUrgentMarkerForNormalTask(t *testing.T) {
	e, pool := traceEngine(t, true)
	f := &stubFetcher{steps: []string{"打开列表页"}}
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}

	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := pool.all(); strings.Contains(got, traceUrgentStep) {
		t.Fatalf("普通任务不该有加急标记：\n%s", got)
	}
}

// 只记第一次尝试：urgent 是「排队位置」的概念，同一次执行里的重试不是新的加急。
func TestRunAttemptUrgentMarkerOnlyOnFirstAttempt(t *testing.T) {
	e, pool := traceEngine(t, true)
	f := &stubFetcher{steps: []string{"打开列表页"}}
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com", Urgent: true}

	for attempt := 0; attempt < 3; attempt++ {
		if err := e.runAttempt(context.Background(), f, task, attempt); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if n := strings.Count(pool.all(), traceUrgentStep); n != 1 {
		t.Fatalf("加急标记应只出现 1 次（第一次尝试），实得 %d 次：\n%s", n, pool.all())
	}
}
