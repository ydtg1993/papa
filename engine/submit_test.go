package engine

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
)

// submitEngine 造一个够跑投递路径的引擎：假库 + 一个已声明且有池子的 "stub" 阶段。
//
// cfg.Crawler.Stages 必须真的有 stub —— SubmitTask 的第一道校验查的就是它（而不是 e.stages）。
func submitEngine(t *testing.T, f *fakeTaskDB, pool *workerpool.WorkerPool[*Task]) *Engine {
	t.Helper()
	cfg := &config.Config{}
	cfg.Crawler.TraceFiles.Dir = t.TempDir()

	e := &Engine{
		// ctx 必须有：阶段没预登记池子时 getOrCreatePool 会懒建一个并 Start，
		// 而转发协程的 select 里要读 e.ctx.Done() —— nil ctx 会直接 panic（不是"卡住"）。
		ctx:        context.Background(),
		db:         openFakeTaskDB(t, f),
		loggerSet:  &loggers.LoggerSet{Engine: quietLogger(), DB: quietLogger()},
		cfg:        cfg,
		stages:     map[string]*stageInfo{"stub:stub": {stage: "stub", config: StageConfig{Site: "stub", Entry: true}}},
		pools:      newPoolRegistry(),
		dedupCache: newDedupCache(0),
	}
	e.breakers.breakerStore = &memoryBreakerRepository{}
	e.breakers.breakerRuntime = true
	e.pools.Set("stub:stub", pool)
	return e
}

// quietLogger 吞掉输出：这些用例故意制造大量失败路径，日志会淹没测试输出。
func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

func queued(t *testing.T, pool *workerpool.WorkerPool[*Task]) int {
	t.Helper()
	main, urgent := pool.QueueDepths()
	return main + urgent
}

/* ---------- 入参校验 ---------- */

// 三道校验都在碰数据库之前：stage/url 为空、阶段没在配置里声明。
// 它们走 logSubmitError，所以框架侧一定会留下日志 —— 调用方忘了记也不会丢。
func TestSubmitTaskValidatesInput(t *testing.T) {
	cases := []struct {
		name string
		task *Task
	}{
		{"stage 为空", &Task{URL: "https://example.com"}},
		{"url 为空", &Task{Stage: "stub"}},
		{"site 为空", &Task{Stage: "stub", URL: "https://example.com"}},
		{"stage 不在配置里", &Task{Stage: "ghost", URL: "https://example.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTaskDB()
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := submitEngine(t, f, pool)

			if err := e.SubmitTask(c.task); err == nil {
				t.Fatal("应当报错")
			}
			if sql := f.readSQL(); sql != "" {
				t.Fatalf("校验失败不该碰数据库：\n%s", sql)
			}
			if queued(t, pool) != 0 {
				t.Fatal("校验失败不该入队")
			}
		})
	}
}

/* ---------- 两阶段去重 ---------- */

// 内存去重命中 + 非轮询：视为成功直接返回，连库都不查。
func TestSubmitTaskDedupHitSkipsEverything(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", Site: "stub", URL: "https://example.com"}
	e.dedupCache.Add(task.Unique())

	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("去重命中应视为成功，实得 %v", err)
	}
	if sql := f.readSQL(); sql != "" {
		t.Fatalf("去重命中不该查库：\n%s", sql)
	}
	if queued(t, pool) != 0 {
		t.Fatal("去重命中不该入队")
	}
}

// 已入库的轮询任务：即便内存去重命中也要回查一次拿 ID，然后继续入队
// （周期轮询靠的就是反复投递同一个 stage|url）。
func TestSubmitTaskRepeatableDedupHitRequeues(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", Site: "stub", URL: "https://example.com", Repeatable: true}
	e.dedupCache.Add(task.Unique())

	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if task.ID != 7 {
		t.Fatalf("应回填库里那行的 ID（假库是 7），实得 %d", task.ID)
	}
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
}

// DB 命中且任务**没带 ID**（全新提交）：去重跳过，不入队。
func TestSubmitTaskExistingRecordWithoutIDIsSkipped(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", Site: "stub", URL: "https://example.com"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if task.ID != 7 {
		t.Fatalf("应回填已有记录的 ID，实得 %d", task.ID)
	}
	if queued(t, pool) != 0 {
		t.Fatal("已存在的非轮询任务不该重复入队")
	}
	// 回查命中后要重新塞回内存去重表，下次可以直接短路
	if !e.dedupCache.Get(task.Unique()) {
		t.Fatal("DB 命中后应把 key 加回内存去重表")
	}
}

// 已带 ID 的重提交（启动恢复 / 后台重投）：必须继续入队 ——
// 把它当"新任务"跳过就等于恢复了个寂寞。
func TestSubmitTaskExistingRecordWithIDRequeues(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
}

// 去重回查只投影 id/status：历史行的 error/meta 可能很宽，去重只要这两列。
func TestFindTaskRecordProjectsIDAndStatus(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	var record models.CrawlerTask
	if err := e.submissions().findTaskRecord(&Task{Stage: "stub", Site: "stub", URL: "https://example.com"}, &record); err != nil {
		t.Fatalf("findTaskRecord = %v", err)
	}
	sql := f.readSQL()
	if strings.Contains(sql, "SELECT *") {
		t.Fatalf("去重回查不该 SELECT *：\n%s", sql)
	}
	for _, col := range []string{"`id`", "`status`"} {
		if !strings.Contains(sql, col) {
			t.Fatalf("去重回查缺列 %s：\n%s", col, sql)
		}
	}
}

// 已到终态（success/failed）的行不再入队：业务侧标失败就是对排队中任务的拦截力。
func TestSubmitTaskTerminalRecordIsSkipped(t *testing.T) {
	for _, status := range []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed} {
		t.Run(statusName(status), func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(status)
			pool := workerpool.NewWorkerPool[*Task](1, 8)
			e := submitEngine(t, f, pool)

			task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"}
			if err := e.SubmitTask(task); err != nil {
				t.Fatalf("SubmitTask = %v", err)
			}
			if queued(t, pool) != 0 {
				t.Fatal("终态任务不该再入队")
			}
		})
	}
}

/* ---------- 全新任务的插入路径 ---------- */

// 全新任务：先落库（拿到 ID）再入队 —— "已入队 ⇒ 行里是 pending" 必须是不变量，
// 反过来的话 worker 会在行还是"成功"的窗口里取到任务，被 claimTask 当成"已被运营改动"跳过。
func TestSubmitTaskNewTaskInsertsBeforeEnqueue(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 库里还没有这一行
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", Site: "stub", URL: "https://example.com/new"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}

	if !strings.Contains(f.written(), "INSERT INTO") {
		t.Fatalf("新任务应先落库：\n%s", f.written())
	}
	if task.ID == 0 {
		t.Fatal("INSERT 应回填主键")
	}
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
}

// 插入撞唯一索引（并发的另一路已经把同样的 stage|url 写进去了）：
// 回查那行的 ID、记进去重表、**不再入队** —— 另一路会负责投递它。
func TestSubmitTaskInsertConflictBackfillsAndSkips(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 第一次查：还没有
	f.failNext = 1  // 让 INSERT 失败（模拟并发撞唯一索引）
	f.onQuery = func(n int) {
		if n >= 2 {
			f.noRows = false // 回查时另一路已经写进去了
		}
	}

	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", Site: "stub", URL: "https://example.com/race"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("并发撞车应当被吸收，而不是报错：%v", err)
	}
	if task.ID != 7 {
		t.Fatalf("应回填库里已有行的 ID（假库是 7），实得 %d", task.ID)
	}
	if queued(t, pool) != 0 {
		t.Fatal("撞车的那条不该重复入队（另一路已经在投递它）")
	}
	if !e.dedupCache.Get(task.Unique()) {
		t.Fatal("撞车后应把 key 加进内存去重表")
	}
}

// 插入失败且回查也查不到：那是真的写不进去（不是并发），必须把错误报出去，
// 不能吞掉 —— 否则业务看到的是一批任务凭空消失。
func TestSubmitTaskInsertFailureIsReported(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	f.failNext = 1

	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/x"})
	if err == nil {
		t.Fatal("写库失败必须报错")
	}
	if !strings.Contains(err.Error(), "insert crawler task to db failed") {
		t.Fatalf("错误信息 = %q", err.Error())
	}
	if queued(t, pool) != 0 {
		t.Fatal("落库失败不该入队")
	}
}

/* ---------- 队列已满 → 溢出到 DB ---------- */

// 队列容量已满：任务保持持久 pending，等待阶段投递器补位，不算失败。
// 对业务表现为"提交成功"，但它不会立刻被 worker 取走。
func TestSubmitTaskAcceptsDurablePendingWhenQueueFull(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 1)
	e := submitEngine(t, f, pool)
	if err := pool.SubmitContext(context.Background(), &Task{ID: 99}); err != nil {
		t.Fatal(err)
	}
	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"}
	done := make(chan error, 1)
	go func() { done <- e.SubmitTask(task) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("persisted admission waited on worker queue")
	}
	if queued(t, pool) != 1 {
		t.Fatal("queue exceeded capacity")
	}
	if f.row["status"] != int64(models.TaskStatusPending) {
		t.Fatal("accepted task lost pending state")
	}
	if strings.Contains(f.written(), "CONCAT(COALESCE(error") {
		t.Fatal("queue pressure marked task failed")
	}
}
func TestSubmitTaskKeepsPendingAfterPoolStop(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	pool.Stop(0)
	e := submitEngine(t, f, pool)
	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if f.row["status"] != int64(models.TaskStatusPending) {
		t.Fatal("stopped pool changed durable pending")
	}
	if !e.dedupCache.Get(task.Unique()) {
		t.Fatal("durable accepted task lost dedup ownership")
	}
	if strings.Contains(f.written(), "CONCAT(COALESCE(error") || strings.Contains(f.written(), "`repeatable`") {
		t.Fatalf("delivery changed unrelated columns: %s", f.written())
	}
}
func TestPendingDeliveryStoppedPoolDoesNotWriteFailure(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)
	pool.Stop(0)
	if err := e.deliverPending(context.Background(), &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"}); err == nil {
		t.Fatal("stopped pool accepted delivery")
	}
	if f.written() != "" {
		t.Fatalf("delivery wrote state: %s", f.written())
	}
}
func TestSubmitToPoolDoesNotRewritePersistedState(t *testing.T) {
	for _, repeatable := range []bool{false, true} {
		f := newFakeTaskDB()
		pool := workerpool.NewWorkerPool[*Task](1, 8)
		e := submitEngine(t, f, pool)
		task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com", Repeatable: repeatable, Meta: map[string]string{"series_id": "1"}}
		e.submitToPool(task, models.CrawlerTask{ID: 7, Status: models.TaskStatusPending, Repeat: 4})
		if f.written() != "" || f.readSQL() != "" {
			t.Fatalf("durable delivery rewrote metadata or round: %s", f.written())
		}
	}
}
func TestSubmitTaskDelayedGoesToHeap(t *testing.T) {
	f := newFakeTaskDB()
	f.row["not_before"] = time.Now().Add(time.Hour)
	f.row["delay_ms"] = int64(time.Hour / time.Millisecond)
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com", Delay: time.Hour}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if queued(t, pool) != 0 {
		t.Fatal("未到点不该入队")
	}

	n := e.delays.Len()

	if n != 0 {
		t.Fatalf("普通持久化延迟任务不应驻留堆，实得 %d", n)
	}
}

// NotBefore 已经是过去时间 → 不延迟，直接入队。
func TestSubmitTaskPastNotBeforeEnqueuesImmediately(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com", NotBefore: time.Now().Add(-time.Minute)}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
}

/* ---------- SubmitTasks 批量 ---------- */

func TestSubmitTasksEmptyAndValidation(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	if err := e.SubmitTasks(nil); err != nil {
		t.Fatalf("空切片应当直接成功：%v", err)
	}
	if err := e.SubmitTasks([]*Task{}); err != nil {
		t.Fatalf("空切片应当直接成功：%v", err)
	}
	if sql := f.readSQL(); sql != "" {
		t.Fatalf("空切片不该碰库：\n%s", sql)
	}

	// 任一条不合法 → 整批拒绝（宁可不投，也不投一半）
	bad := []*Task{
		{Stage: "stub", Site: "stub", URL: "https://ok.example.com"},
		{Stage: "ghost", URL: "https://bad.example.com"},
	}
	if err := e.SubmitTasks(bad); err == nil {
		t.Fatal("含非法阶段的一批应当报错")
	}
	if queued(t, pool) != 0 {
		t.Fatal("校验失败时一条都不该入队")
	}
}

func TestSubmitTasksBatchInsertsThenEnqueues(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 两条都是全新的
	f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	tasks := []*Task{
		{Stage: "stub", Site: "stub", URL: "https://example.com/a"},
		{Stage: "stub", Site: "stub", URL: "https://example.com/b"},
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}

	// 快路径是一条多行 INSERT（批量提交本来就是冲着减少 DB 往返来的）
	inserts := strings.Count(f.written(), "INSERT INTO")
	if inserts < 1 {
		t.Fatalf("应至少有一条 INSERT：\n%s", f.written())
	}
	for i, task := range tasks {
		if task.ID == 0 {
			t.Fatalf("第 %d 条没回填 ID", i)
		}
	}
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
}

func TestSubmitTasksSkipsDedupedRows(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	dup := &Task{Stage: "stub", Site: "stub", URL: "https://example.com/dup"}
	fresh := &Task{Stage: "stub", Site: "stub", URL: "https://example.com/fresh"}
	e.dedupCache.Add(dup.Unique()) // 内存里已有 → 非轮询直接跳过
	f.noRows = true                // fresh 在库里是全新的
	f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }

	if err := e.SubmitTasks([]*Task{dup, fresh}); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
	if !e.dedupCache.Get(dup.Unique()) || !e.dedupCache.Get(fresh.Unique()) {
		t.Fatal("两条的 key 都应在内存去重表里")
	}
}

func TestSubmitTasksHonoursDelay(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 两条都当作全新任务，才会走到"延迟 vs 立即"这一步
	f.onQuery = func(n int) { f.noRows = !strings.Contains(f.queries[n-1], "WHERE id = ?") }
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	tasks := []*Task{
		{Stage: "stub", Site: "stub", URL: "https://example.com/now"},
		{Stage: "stub", Site: "stub", URL: "https://example.com/later", Delay: time.Hour},
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}

	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}

	n := e.delays.Len()

	if n != 0 {
		t.Fatalf("普通持久化延迟任务不应驻留堆，实得 %d", n)
	}
}

// 批量插入失败要退到逐条（一行冲突不能让整批一条都进不去），
// 且真正"已存在"的那几条不该被重复入队。
func TestSubmitTasksSkipsConflictingRows(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 去重那两次查库：都没有
	f.failNext = 2  // 批量 INSERT + 第一条逐条 INSERT 都失败
	// 第 3 次查询是逐条失败后的 (stage,url) 回查 —— 这时库里已经有那行了（另一路写的）
	f.onQuery = func(n int) {
		if n >= 3 {
			f.noRows = false
		}
	}

	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	tasks := []*Task{
		{Stage: "stub", Site: "stub", URL: "https://example.com/conflict"},
		{Stage: "stub", Site: "stub", URL: "https://example.com/ok"},
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}
	if !strings.Contains(f.written(), "INSERT INTO") {
		t.Fatalf("应当退到逐条插入：\n%s", f.written())
	}
	// 冲突那条被跳过（库里那行由别人投递），剩下的正常入队
	requirePendingNotification(t, e, "stub:stub")
	if queued(t, pool) != 0 {
		t.Fatal("acceptance synchronously enqueued work")
	}
	if tasks[0].ID != 7 {
		t.Fatalf("冲突那条应回填库里已有行的 ID，实得 %d", tasks[0].ID)
	}
}

func statusName(s models.TaskStatus) string {
	switch s {
	case models.TaskStatusPending:
		return "pending"
	case models.TaskStatusProcessing:
		return "processing"
	case models.TaskStatusSuccess:
		return "success"
	case models.TaskStatusFailed:
		return "failed"
	}
	return "unknown"
}

// 新任务带了周期：只在这里（首次入库）播种，之后要改周期走 SetTaskRepeatInterval / 后台动作。
func TestSubmitTaskSeedsRepeatInterval(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 库里还没有这一行 → 走 INSERT
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)

	task := &Task{
		Stage: "stub", Site: "stub", URL: "https://example.com/series/1",
		Repeatable: true, RepeatInterval: 10 * time.Minute,
	}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if !strings.Contains(f.written(), "`repeat_interval`") {
		t.Fatalf("首次入库应播种 repeat_interval：\n%s", f.written())
	}
	if args := f.writtenArgs(); !strings.Contains(args, "600") {
		t.Fatalf("周期应按秒落库（10m → 600）：%s", args)
	}
}

/* ---------- 提交前的落库写失败 → 不入队 ---------- */

// 已有 pending 重复提交不需要再次改写 status，也不应因额外状态 UPDATE 失败而丢失受理。
func TestSubmitExistingPendingNeedsNoStatusUpdate(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)
	f.failNext = 1
	task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatal(err)
	}
	if f.written() != "" {
		t.Fatalf("pending handoff required redundant UPDATE: %s", f.written())
	}
	if _, err := e.recoverPendingStage("stub:stub"); err != nil {
		t.Fatal(err)
	}
	if queued(t, pool) != 1 {
		t.Fatal("pending task not recovered")
	}
	if f.written() != "" {
		t.Fatal("recovery rewrote state")
	}
}
func TestSubmitToPoolNeverResetsProcessingSnapshot(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := submitEngine(t, f, pool)
	task := &Task{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com"}
	e.submitToPool(task, models.CrawlerTask{ID: 7, Status: models.TaskStatusProcessing})
	if f.written() != "" || queued(t, pool) != 0 {
		t.Fatal("processing snapshot reactivated")
	}
}

func requirePendingNotification(t *testing.T, e *Engine, key string) {
	t.Helper()
	select {
	case <-e.deliveryState().Notifications(key):
	default:
		t.Fatal("durable pending did not notify stage dispatcher")
	}
}
