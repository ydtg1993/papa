package engine

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
)

// submitEngine 造一个够跑投递路径的引擎：假库 + 一个已声明且有池子的 "stub" 阶段。
//
// cfg.Crawler.Stages 必须真的有 stub —— SubmitTask 的第一道校验查的就是它（而不是 e.stages）。
func submitEngine(t *testing.T, f *fakeTaskDB, pool *workerpool.WorkerPool[*Task]) *Engine {
	t.Helper()
	cfg := &config.Config{}

	e := &Engine{
		db:         openFakeTaskDB(t, f),
		loggerSet:  &loggers.LoggerSet{Engine: quietLogger(), DB: quietLogger()},
		cfg:        cfg,
		stages:     map[string]*stageInfo{"stub": {workerPool: pool}},
		dedupCache: newDedupCache(0),
		spilled:    make(map[string][]*Task),
		delayCh:    make(chan struct{}, 1),
	}
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
		{"stage 不在配置里", &Task{Stage: "ghost", URL: "https://example.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTaskDB()
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
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
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com"}
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
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com", Repeatable: true}
	e.dedupCache.Add(task.Unique())

	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if task.ID != 7 {
		t.Fatalf("应回填库里那行的 ID（假库是 7），实得 %d", task.ID)
	}
	if queued(t, pool) != 1 {
		t.Fatalf("轮询任务应继续入队，实得 %d 条", queued(t, pool))
	}
}

// DB 命中且任务**没带 ID**（全新提交）：去重跳过，不入队。
func TestSubmitTaskExistingRecordWithoutIDIsSkipped(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com"}
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
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if queued(t, pool) != 1 {
		t.Fatalf("带 ID 的重提交应入队，实得 %d 条", queued(t, pool))
	}
}

// 已到终态（success/failed）的行不再入队：业务侧标失败就是对排队中任务的拦截力。
func TestSubmitTaskTerminalRecordIsSkipped(t *testing.T) {
	for _, status := range []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed} {
		t.Run(statusName(status), func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["status"] = int64(status)
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := submitEngine(t, f, pool)

			task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}
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
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com/new"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}

	if !strings.Contains(f.written(), "INSERT INTO") {
		t.Fatalf("新任务应先落库：\n%s", f.written())
	}
	if task.ID == 0 {
		t.Fatal("INSERT 应回填主键")
	}
	if queued(t, pool) != 1 {
		t.Fatalf("新任务应入队，实得 %d 条", queued(t, pool))
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

	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com/race"}
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

	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/x"})
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

// 队列达高水位：任务保持 pending、进溢出列表由 drain 回灌，**不算失败**。
// 对业务表现为"提交成功"，但它不会立刻被 worker 取走。
func TestSubmitTaskSpillsWhenQueueAtWatermark(t *testing.T) {
	f := newFakeTaskDB()
	// 容量 4、水位 0.25 → 1 条就满
	pool := workerpool.NewWorkerPool[*Task](1, 4, 0.25)
	e := submitEngine(t, f, pool)

	if err := pool.Submit(&Task{URL: "occupy", Stage: "stub"}); err != nil {
		t.Fatalf("占位提交 = %v", err)
	}

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com/spill"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("溢出不该报错：%v", err)
	}

	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("溢出列表应有 1 条，实得 %d", got)
	}
	if got := e.spilledCount.Load(); got != 1 {
		t.Fatalf("溢出计数 = %d, want 1（监控页靠它看漏了多少）", got)
	}
}

/* ---------- 投递失败 → 回滚 ---------- */

// 池子已停机时的投递失败：回滚内存去重表 + 把行标成 failed 并记下原因。
// 不标的话它会永远停在 pending，而没有任何队列会再捞 pending 的普通任务。
func TestSubmitTaskRollsBackOnSubmitFailure(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	pool.Stop(time.Second) // 停掉，让 Submit 必然失败
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}
	err := e.SubmitTask(task)
	if err == nil {
		t.Fatal("投递失败必须报错")
	}
	if e.dedupCache.Get(task.Unique()) {
		t.Fatal("投递失败应把 key 从内存去重表里删掉，否则这条再也投不进来")
	}
	if n := strings.Count(f.written(), "UPDATE"); n != 2 {
		t.Fatalf("应有两次 UPDATE（先标 pending、失败后标 failed），实得 %d：\n%s", n, f.written())
	}
	if !strings.Contains(f.writtenArgs(), "already stopped") {
		t.Fatalf("失败原因应写进 error 列：%s", f.writtenArgs())
	}
	// 回滚也只写自己负责的列：整行写回会把运营在这期间改的 repeatable（后台「开/停轮询」）盖掉
	if strings.Contains(f.written(), "`repeatable`") {
		t.Fatalf("提交失败的回滚不该整行写回：\n%s", f.written())
	}
}

/* ---------- 投递只写自己负责的列 ---------- */

// 投递路径的 UPDATE 不得整行写回（原先的 e.db.Save(record) 会）：record 是更早 SELECT 出来的
// 快照，延迟投递那条会把它压在 delayHeap 里直到 Delay 到点 —— 整行写回会把运营在这期间改的列
// （repeatable / urgent / error…）静默盖回去。尤其 repeatable 现在由后台「开/停轮询」随时改，
// 被盖回去就是"停轮询被数据面吃掉、队列永远继续轮询"。
func TestSubmitToPoolWritesOnlyItsOwnColumns(t *testing.T) {
	cases := []struct {
		name           string
		repeatable     bool
		wantRepeatIncr bool
	}{
		{"普通任务：只标 pending", false, false},
		{"轮询任务：顺带把轮询代数 +1", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := submitEngine(t, f, pool)

			task := &Task{
				ID: 7, Stage: "stub", URL: "https://example.com",
				Repeatable: tc.repeatable, Site: "s1",
				Meta: map[string]string{"series_id": "1"},
			}
			if err := e.SubmitTask(task); err != nil {
				t.Fatalf("SubmitTask = %v", err)
			}
			sql := f.written()
			// `` `repeat` `` 与 `` `repeatable` `` 不会互相误命中（尾引号），所以两条都要断言
			for _, never := range []string{
				"`repeatable`", "`url`", "`title`", "`content`", "`urgent`", "`error`",
				// 周期与排期只由首次插入（toModel）与「设轮询周期」/「开轮询」写：
				// 提交路径碰它们就等于把运营改的盖回去
				"`repeat_interval`", "`next_repeat_at`", "`last_repeat_at`",
			} {
				if strings.Contains(sql, never) {
					t.Fatalf("投递不该写 %s：\n%s", never, sql)
				}
			}
			if got := strings.Contains(sql, "`repeat`"); got != tc.wantRepeatIncr {
				t.Fatalf("repeat 自增 = %v, want %v：\n%s", got, tc.wantRepeatIncr, sql)
			}
			for _, want := range []string{"`status`", "`meta`", "`site`"} {
				if !strings.Contains(sql, want) {
					t.Fatalf("投递应写 %s：\n%s", want, sql)
				}
			}
		})
	}
}

/* ---------- 延迟投递 ---------- */

// 未到点的任务先进延迟堆，不占 worker 的并发位；到点由 dispatcher 统一入队。
func TestSubmitTaskDelayedGoesToHeap(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com", Delay: time.Hour}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if queued(t, pool) != 0 {
		t.Fatal("未到点不该入队")
	}

	e.delayMu.Lock()
	n := e.delayHeap.Len()
	e.delayMu.Unlock()
	if n != 1 {
		t.Fatalf("延迟堆里应有 1 条，实得 %d", n)
	}
}

// NotBefore 已经是过去时间 → 不延迟，直接入队。
func TestSubmitTaskPastNotBeforeEnqueuesImmediately(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com", NotBefore: time.Now().Add(-time.Minute)}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if queued(t, pool) != 1 {
		t.Fatalf("过期时间点应立刻入队，实得 %d 条", queued(t, pool))
	}
}

/* ---------- SubmitTasks 批量 ---------- */

func TestSubmitTasksEmptyAndValidation(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
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
		{Stage: "stub", URL: "https://ok.example.com"},
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
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	tasks := []*Task{
		{Stage: "stub", URL: "https://example.com/a"},
		{Stage: "stub", URL: "https://example.com/b"},
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
	if got := queued(t, pool); got != 2 {
		t.Fatalf("两条都该入队，实得 %d 条", got)
	}
}

func TestSubmitTasksSkipsDedupedRows(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	dup := &Task{Stage: "stub", URL: "https://example.com/dup"}
	fresh := &Task{Stage: "stub", URL: "https://example.com/fresh"}
	e.dedupCache.Add(dup.Unique()) // 内存里已有 → 非轮询直接跳过
	f.noRows = true                // fresh 在库里是全新的

	if err := e.SubmitTasks([]*Task{dup, fresh}); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}
	if queued(t, pool) != 1 {
		t.Fatalf("只该入队没被去重的那条，实得 %d 条", queued(t, pool))
	}
	if !e.dedupCache.Get(dup.Unique()) || !e.dedupCache.Get(fresh.Unique()) {
		t.Fatal("两条的 key 都应在内存去重表里")
	}
}

func TestSubmitTasksHonoursDelay(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 两条都当作全新任务，才会走到"延迟 vs 立即"这一步
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	tasks := []*Task{
		{ID: 7, Stage: "stub", URL: "https://example.com/now"},
		{ID: 7, Stage: "stub", URL: "https://example.com/later", Delay: time.Hour},
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}

	if queued(t, pool) != 1 {
		t.Fatalf("只有非延迟的那条该立刻入队，实得 %d 条", queued(t, pool))
	}
	e.delayMu.Lock()
	n := e.delayHeap.Len()
	e.delayMu.Unlock()
	if n != 1 {
		t.Fatalf("延迟堆里应有 1 条，实得 %d", n)
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

	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	tasks := []*Task{
		{Stage: "stub", URL: "https://example.com/conflict"},
		{Stage: "stub", URL: "https://example.com/ok"},
	}
	if err := e.SubmitTasks(tasks); err != nil {
		t.Fatalf("SubmitTasks = %v", err)
	}
	if !strings.Contains(f.written(), "INSERT INTO") {
		t.Fatalf("应当退到逐条插入：\n%s", f.written())
	}
	// 冲突那条被跳过（库里那行由别人投递），剩下的正常入队
	if got := queued(t, pool); got != 1 {
		t.Fatalf("只该入队未冲突的那条，实得 %d 条", got)
	}
	if tasks[0].ID != 7 {
		t.Fatalf("冲突那条应回填库里已有行的 ID，实得 %d", tasks[0].ID)
	}
}

/* ---------- ReSubmitTask ---------- */

func TestReSubmitTaskValidatesInput(t *testing.T) {
	cases := []struct {
		name string
		task *Task
	}{
		{"stage 为空", &Task{URL: "https://example.com"}},
		{"url 为空", &Task{Stage: "stub"}},
		{"stage 不在配置里", &Task{Stage: "ghost", URL: "https://example.com"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTaskDB()
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := submitEngine(t, f, pool)

			if err := e.ReSubmitTask(c.task); err == nil {
				t.Fatal("应当报错")
			}
			if sql := f.readSQL(); sql != "" {
				t.Fatalf("校验失败不该碰库：\n%s", sql)
			}
		})
	}
}

func TestReSubmitTaskMissingRecord(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	err := e.ReSubmitTask(&Task{Stage: "stub", URL: "https://example.com/none"})
	if err == nil {
		t.Fatal("库里没有这行时应当报错")
	}
	if !strings.Contains(err.Error(), "record not exists") {
		t.Fatalf("错误信息 = %q", err.Error())
	}
}

// 重提交不走 SubmitTask 的去重/落库那套，只按 (url, stage) 找回 ID 后直接投递。
func TestReSubmitTaskEnqueuesExistingRecord(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com"}
	if err := e.ReSubmitTask(task); err != nil {
		t.Fatalf("ReSubmitTask = %v", err)
	}
	if task.ID != 7 {
		t.Fatalf("应回填库里那行的 ID，实得 %d", task.ID)
	}
	if queued(t, pool) != 1 {
		t.Fatalf("应入队 1 条，实得 %d", queued(t, pool))
	}
	// 回查必须按 url + stage（与唯一索引一致）
	read := f.readSQL()
	for _, want := range []string{"url_hash = ?", "stage = ?"} {
		if !strings.Contains(read, want) {
			t.Fatalf("回查应带 %s 条件：\n%s", want, read)
		}
	}
}

func TestReSubmitTaskRollsBackOnSubmitFailure(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	pool.Stop(time.Second)
	e := submitEngine(t, f, pool)

	task := &Task{Stage: "stub", URL: "https://example.com"}
	err := e.ReSubmitTask(task)
	if err == nil {
		t.Fatal("投递失败必须报错")
	}
	if e.dedupCache.Get(task.Unique()) {
		t.Fatal("失败时应把 key 从内存去重表里删掉")
	}
	if !strings.Contains(f.written(), "UPDATE") {
		t.Fatalf("失败时应把行标成 failed：\n%s", f.written())
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
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{
		Stage: "stub", URL: "https://example.com/series/1",
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
