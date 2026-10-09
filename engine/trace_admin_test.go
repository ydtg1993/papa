package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
)

// traceDBEngine 造一个追踪开着、并且认得 crawler_task_trace 的引擎。
func traceDBEngine(t *testing.T, f *fakeTaskDB) *Engine {
	t.Helper()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cfg.Crawler.Trace.Enabled = true
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	return e
}

/* ---------- ListTrace ---------- */

// 追踪没开时接口要给出**能看懂的原因**：后台抽屉会把这句原样显示给运营，
// 而不是让他对着 404 猜。
func TestListTraceDisabled(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cfg.Crawler.Trace.Enabled = false

	steps, err := e.ListTrace(7)
	if err == nil {
		t.Fatal("追踪没开时应当报错")
	}
	if !strings.Contains(err.Error(), "crawler.trace.enabled") {
		t.Fatalf("错误信息应指出是哪个开关：%v", err)
	}
	if steps != nil {
		t.Fatalf("报错时不该给步骤，实得 %+v", steps)
	}
	if sql := f.readSQL(); sql != "" {
		t.Fatalf("开关没开不该查库：\n%s", sql)
	}
}

// 步骤记录按（尝试、步骤）排序返回，并把库里的 status 枚举翻成前端用的 ok/failed。
func TestListTraceMapsRows(t *testing.T) {
	f := newFakeTaskDB()
	f.traceRows = []fakeRow{
		traceRow(1, 7, 0, 0, "打开页面", int64(models.TraceOK), []byte(`{"url":"a"}`)),
		traceRow(2, 7, 0, 1, "解析列表", int64(models.TraceFailed), nil),
		traceRow(3, 7, 1, 0, "打开页面", int64(models.TraceOK), nil),
		traceRow(4, 7, 1, 1, "下载封面", int64(models.TraceWarn), []byte(`{"cover_url":"u"}`)),
	}
	e := traceDBEngine(t, f)

	steps, err := e.ListTrace(7)
	if err != nil {
		t.Fatalf("ListTrace = %v", err)
	}
	if len(steps) != 4 {
		t.Fatalf("应返回 4 步，实得 %d", len(steps))
	}
	if steps[0].Step != "打开页面" || steps[0].Status != "ok" || steps[0].Attempt != 0 || steps[0].Seq != 0 {
		t.Fatalf("第一步 = %+v", steps[0])
	}
	if steps[1].Status != "failed" {
		t.Fatalf("失败步骤的 status 应翻成 failed：%+v", steps[1])
	}
	if steps[1].Duration != 3*time.Millisecond {
		t.Fatalf("耗时没带出来：%+v", steps[1])
	}
	if got := string(steps[0].Data); got != `{"url":"a"}` {
		t.Fatalf("data 应原样透出 JSON 文本，实得 %q", got)
	}
	if steps[2].Attempt != 1 {
		t.Fatalf("重试那次的步骤 attempt 应为 1：%+v", steps[2])
	}
	// 非致命档翻成 warn（不是 failed）：后台因此能把它和"真的失败了"分开显示
	if steps[3].Status != "warn" {
		t.Fatalf("警告步骤的 status 应翻成 warn：%+v", steps[3])
	}
	if got := string(steps[3].Data); got != `{"cover_url":"u"}` {
		t.Fatalf("警告步的 data 应原样透出：%q", got)
	}

	// 查询要按 task_id 过滤、按 attempt,seq 排序、并带上限
	read := f.readSQL()
	for _, want := range []string{"task_id = ?", "ORDER BY attempt,seq", "LIMIT"} {
		if !strings.Contains(strings.ReplaceAll(read, " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Fatalf("查询缺少 %q：\n%s", want, read)
		}
	}
}

// 一条步骤都没有（任务还没跑过 / 记录被清理了）不算错误，返回空列表。
func TestListTraceEmpty(t *testing.T) {
	f := newFakeTaskDB()
	e := traceDBEngine(t, f)

	steps, err := e.ListTrace(7)
	if err != nil {
		t.Fatalf("ListTrace = %v", err)
	}
	if len(steps) != 0 {
		t.Fatalf("应返回空列表，实得 %+v", steps)
	}
	if steps == nil {
		t.Fatal("应返回空切片而不是 nil（JSON 里才是 [] 而不是 null）")
	}
}

// 最常见的现场事故是"开关开着但表没建"。这时要报出可追查的错误，而不是静默空列表。
func TestListTraceReportsDBError(t *testing.T) {
	f := newFakeTaskDB()
	f.failQueries = 1
	e := traceDBEngine(t, f)

	_, err := e.ListTrace(7)
	if err == nil {
		t.Fatal("查库失败应当报错")
	}
	if !strings.Contains(err.Error(), "load trace of task 7") {
		t.Fatalf("错误信息应带上任务 ID：%v", err)
	}
}

/* ---------- 保留期清理 ---------- */

// 按批删：删满一批说明后面还有，接着删；删不满就停。避免一条 DELETE 长时间锁表。
func TestCleanupTraceDeletesInBatches(t *testing.T) {
	f := newFakeTaskDB()
	f.affected = traceDeleteBatch // 第一批删满
	f.onExec = func(n int) {
		if n >= 2 {
			f.affected = 5 // 第二批没删满 → 收工
		}
	}
	e := traceDBEngine(t, f)

	e.cleanupTrace(time.Hour)

	if got := strings.Count(f.written(), "DELETE"); got != 2 {
		t.Fatalf("应删两批（第一批满、第二批不满），实得 %d：\n%s", got, f.written())
	}
	// 删除条件必须按 created_at 范围 + LIMIT，否则是全表长事务
	sql := f.written()
	for _, want := range []string{"created_at < ?", "LIMIT"} {
		if !strings.Contains(strings.ReplaceAll(sql, " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Fatalf("删除语句缺少 %q：\n%s", want, sql)
		}
	}
}

// 删一次就删不满（常态）：一批收工。
func TestCleanupTraceSingleBatch(t *testing.T) {
	f := newFakeTaskDB()
	f.affected = 3
	e := traceDBEngine(t, f)

	e.cleanupTrace(time.Hour)
	if got := strings.Count(f.written(), "DELETE"); got != 1 {
		t.Fatalf("删不满一批就该收工，实得 %d 条 DELETE", got)
	}
}

// 删库里出错只记日志、不往外抛：它是后台巡检，不该影响任何业务路径。
func TestCleanupTraceAbortsOnDBError(t *testing.T) {
	f := newFakeTaskDB()
	f.failNext = 1
	e := traceDBEngine(t, f)

	e.cleanupTrace(time.Hour) // 不该 panic
	if got := strings.Count(f.written(), "DELETE"); got != 1 {
		t.Fatalf("出错后不该继续删，实得 %d 条 DELETE", got)
	}
}

// 积压极大时这一轮会连删很多批，Engine.Stop 得能打断它。
func TestCleanupTraceAbortsWhenCtxCancelled(t *testing.T) {
	f := newFakeTaskDB()
	f.affected = traceDeleteBatch // 每批都删满，不打断就是死循环
	e := traceDBEngine(t, f)
	e.cancel()

	e.cleanupTrace(time.Hour)

	if got := strings.Count(f.written(), "DELETE"); got != 0 {
		t.Fatalf("ctx 已取消时不该再删，实得 %d 条 DELETE", got)
	}
}

// 追踪关着 / 保留期为负（显式要求永久保留）时不启动巡检协程。
func TestStartTraceCleanupSkipsWhenIrrelevant(t *testing.T) {
	f := newFakeTaskDB()
	e := traceDBEngine(t, f)

	e.cfg.Crawler.Trace.Enabled = false
	e.startTraceCleanup()

	e.cfg.Crawler.Trace.Enabled = true
	e.cfg.Crawler.Trace.Retention = -time.Hour // 负数 = 永久保留
	e.startTraceCleanup()

	// 两种情况下都不该删任何东西
	if got := f.written(); got != "" {
		t.Fatalf("不该有任何写入：\n%s", got)
	}
}

/* ---------- 一次尝试的收尾 ---------- */

// 一次尝试结束后由 defer 兜底落库：正常返回与 panic 展开都走这条路。
// 这里用真 worker 跑一遍，覆盖 runAttempt → setResult → finish 的完整链路。
func TestRunAttemptWritesStepsThroughFinish(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	cfg := &config.Config{}
	cfg.Crawler.Stages = map[string]config.StageConfig{"stub": {WorkerCount: 1, QueueSize: 8}}
	cfg.Crawler.Trace.Enabled = true
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)

	fetcher := &stubFetcher{steps: []string{"打开页面", "解析列表"}}
	e.AddStage("stub", StageConfig{MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/traced"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}

	// 等追踪落库（一条多行 INSERT）
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(f.written(), "crawler_task_trace") {
		if time.Now().After(deadline) {
			t.Fatalf("步骤没有落库：\n%s", f.written())
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := strings.Count(f.written(), "INSERT INTO `crawler_task_trace`"); got != 1 {
		t.Fatalf("一次尝试应当只写一条多行 INSERT，实得 %d：\n%s", got, f.written())
	}
	args := f.writtenArgs()
	for _, want := range []string{"打开页面", "解析列表"} {
		if !strings.Contains(args, want) {
			t.Fatalf("落库参数里缺少步骤 %q：%s", want, args)
		}
	}

	e.Stop(2 * time.Second)
}
