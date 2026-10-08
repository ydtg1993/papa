package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/datatypes"
)

// queueEngine 在 submitEngine 之上补齐治理队列需要的那几样：
// 运行快照表、累计计数指针、以及可取消的 ctx。
func queueEngine(t *testing.T, f *fakeTaskDB, pool *workerpool.WorkerPool[*Task]) *Engine {
	t.Helper()
	e := submitEngine(t, f, pool)
	e.queueRuns = newQueueRuns()
	e.queueCounters = map[string]*atomic.Int64{
		QueueError:  &e.errorRetriedCount,
		QueueRepeat: &e.repeatRepolledCount,
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	return e
}

// setRows 把假库切成多行模式，造 n 行指定阶段/状态的任务。
// actionRow 造一条待治理队列处理的任务行。
func actionRow(id uint, stage string) *models.CrawlerTask {
	return &models.CrawlerTask{
		ID: id, Stage: stage, URL: fmt.Sprintf("https://example.com/%d", id),
		Status: models.TaskStatusFailed, Content: datatypes.JSON("{}"),
	}
}

func setRows(f *fakeTaskDB, n int, stage string, status models.TaskStatus) {
	for i := 1; i <= n; i++ {
		r := f.rowWithID(int64(i))
		r["stage"] = stage
		r["url"] = fmt.Sprintf("https://example.com/%d", i)
		r["status"] = int64(status)
		f.rows = append(f.rows, r)
	}
}

/* ---------- 失败队列（error_queue） ---------- */

// 失败任务的查询条件：只捞 failed；配了 max_retry 时再按 reprocess 代数过滤。
func TestErrorQueueQueryShape(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cfg.ErrorQueue = config.ErrorQueueConfig{MaxRetry: 3}

	e.errorQueueQuery()().Find(&[]models.CrawlerTask{})
	read := f.readSQL()
	if !strings.Contains(read, "status = ?") {
		t.Fatalf("应只捞 failed：\n%s", read)
	}
	if !strings.Contains(read, "reprocess < ?") {
		t.Fatalf("配了 max_retry 就该按 reprocess 过滤：\n%s", read)
	}

	// MaxRetry=0 表示不限代数 → 不带 reprocess 条件
	f2 := newFakeTaskDB()
	e2 := queueEngine(t, f2, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e2.errorQueueQuery()().Find(&[]models.CrawlerTask{})
	if strings.Contains(f2.readSQL(), "reprocess < ?") {
		t.Fatalf("max_retry=0 表示不限，不该有 reprocess 条件：\n%s", f2.readSQL())
	}
}

// 一整批失败任务被重新投递：状态重置为 pending、代数 +1、计数累加。
func TestProcessErrorQueueRequeuesFailedTasks(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 3, "stub", models.TaskStatusFailed)
	pool := workerpool.NewWorkerPool[*Task](1, 16, 1)
	e := queueEngine(t, f, pool)
	e.cfg.ErrorQueue = config.ErrorQueueConfig{BatchSize: 10, WorkerCount: 2}

	n, err := e.ProcessErrorQueue()
	if err != nil {
		t.Fatalf("ProcessErrorQueue = %v", err)
	}
	if n != 3 {
		t.Fatalf("应重投 3 条，实得 %d", n)
	}
	if got := e.errorRetriedCount.Load(); got != 3 {
		t.Fatalf("累计重投计数 = %d, want 3", got)
	}
	main, urgent := pool.QueueDepths()
	if main+urgent != 3 {
		t.Fatalf("三条都该进队列，实得 %d", main+urgent)
	}

	writes := f.written()
	if !strings.Contains(writes, "reprocess") {
		t.Fatalf("重置时应把 reprocess 代数 +1：\n%s", writes)
	}
	if !strings.Contains(writes, "`retry`") {
		t.Fatalf("重置时应把 retry 归零：\n%s", writes)
	}

	// 运行快照：跑过一次、上次处理 3 条、没留错误
	st := e.GetQueueStats()[QueueError]
	if st.Running {
		t.Fatal("跑完了不该还标着 running")
	}
	if st.Runs != 1 || st.LastProcessed != 3 || st.LastError != "" {
		t.Fatalf("队列快照 = %+v", st)
	}
	if st.TotalProcessed != 3 {
		t.Fatalf("TotalProcessed = %d, want 3", st.TotalProcessed)
	}
}

// 阶段没注册（配置里删掉了这个阶段，库里还留着它的行）→ 跳过，
// 且**不该**把行标成失败 —— 否则版本升级时一批历史行会被无端改坏。
func TestRequeueFailedTaskSkipsUnregisteredStage(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	row := actionRow(3, "ghost")
	if e.requeueFailedTask(row) {
		t.Fatal("未注册的阶段应当跳过")
	}
	if got := f.written(); got != "" {
		t.Fatalf("跳过的行不该被改写：\n%s", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("跳过的行不该入队")
	}
}

// 重新投递失败时，必须把行标成 failed 并写下原因。
// 上面刚把行重置成 pending，不管的话本队列（只捞 failed）再也捞不到它 ——
// 运营看着是"排队中"，实际早就没人会执行。
func TestRequeueFailedTaskMarksRowWhenSubmitFails(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	pool.Stop(0) // 停掉，让 Submit 必然失败
	e := queueEngine(t, f, pool)

	row := actionRow(3, "stub")
	if e.requeueFailedTask(row) {
		t.Fatal("投递失败应返回 false")
	}
	if got := e.errorRetriedCount.Load(); got != 0 {
		t.Fatalf("没投成功不该计数，实得 %d", got)
	}

	writes := f.written()
	if !strings.Contains(writes, "status <> ?") {
		t.Fatalf("标记失败应带 `status <> failed` 守卫（免得重复贴一行原因）：\n%s", writes)
	}
	if !strings.Contains(f.writtenArgs(), QueueError) {
		t.Fatalf("原因里应写明是哪条队列写的：%s", f.writtenArgs())
	}
}

/* ---------- 启动恢复（recover_queue） ---------- */

// 恢复集合是「未到终态」（pending / processing）—— 不看 updated_at：
// 进程刚起时 processing 全是上一次留下的孤儿，这是确定的，不需要靠时间推测。
func TestRecoverQueueQueryShape(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.recoverQueueQuery()().Find(&[]models.CrawlerTask{})
	read := f.readSQL()
	if !strings.Contains(read, "status IN") {
		t.Fatalf("应按 status IN (pending, processing) 捞：\n%s", read)
	}
	if strings.Contains(read, "updated_at") {
		t.Fatalf("不该再用 updated_at 这个启发式：\n%s", read)
	}
}

func TestRequeueRecoverTaskResubmits(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	row.Status = models.TaskStatusProcessing

	if !e.requeueRecoverTask(row) {
		t.Fatal("已注册的阶段应当恢复成功")
	}
	if got := e.recoveredCount.Load(); got != 1 {
		t.Fatalf("恢复计数 = %d, want 1", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("恢复的任务应入队，实得 %d", main+urgent)
	}
	// 恢复要照抄行上的 Repeatable / Urgent，丢了会让轮询任务恢复后不再参与周期轮询
	writes := f.written()
	if !strings.Contains(writes, "`status`") {
		t.Fatalf("应先把行重置为 pending：\n%s", writes)
	}
}

// 恢复前必须把 key 从内存去重表里剔掉：不剔的话 SubmitTask 第一道
// dedupCache.Get 就命中，非轮询任务直接 return nil —— 队列根本没进。
func TestRequeueRecoverTaskClearsDedupCache(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	key := (&Task{Stage: "stub", URL: row.URL}).Unique()
	e.dedupCache.Add(key)

	if !e.requeueRecoverTask(row) {
		t.Fatal("剔除缓存后应当能恢复成功")
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("应入队 1 条，实得 %d（被 dedupCache 挡住就是 0）", main+urgent)
	}
}

func TestRequeueRecoverTaskSkipsUnregisteredStage(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	row := actionRow(3, "ghost")
	if e.requeueRecoverTask(row) {
		t.Fatal("未注册的阶段应当跳过")
	}
	if got := e.recoveredCount.Load(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
}

func TestProcessRecoverQueueProcessesAllActive(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 2, "stub", models.TaskStatusProcessing)
	pool := workerpool.NewWorkerPool[*Task](1, 16, 1)
	e := queueEngine(t, f, pool)
	e.cfg.RecoverQueue = config.RecoverQueueConfig{BatchSize: 10, WorkerCount: 1}

	n, err := e.ProcessRecoverQueue()
	if err != nil {
		t.Fatalf("ProcessRecoverQueue = %v", err)
	}
	if n != 2 {
		t.Fatalf("应恢复 2 条，实得 %d", n)
	}
	if got := e.recoveredCount.Load(); got != 2 {
		t.Fatalf("恢复计数 = %d, want 2", got)
	}
}

// 恢复没开时不该起协程、也不该捞任何东西。
func TestStartRecoverQueueDisabled(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)
	e.cfg.RecoverQueue = config.RecoverQueueConfig{Enabled: false}

	e.startRecoverQueue()
	if got := e.recoveredCount.Load(); got != 0 {
		t.Fatalf("关掉后不该恢复任何任务，实得 %d", got)
	}
}

/* ---------- 周期轮询（repeat_queue） ---------- */

// 轮询集合：已完成的（success/failed）repeatable 任务。
// 不碰还在 pending/processing 的 —— 那些归 recover_queue，免得双入队。
func TestRepeatQueueQueryShape(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.repeatQueueQuery()().Find(&[]models.CrawlerTask{})
	read := f.readSQL()
	if !strings.Contains(read, "repeatable = ?") {
		t.Fatalf("应只捞 repeatable：\n%s", read)
	}
	if !strings.Contains(read, "status IN") {
		t.Fatalf("应限定 success/failed：\n%s", read)
	}
	if strings.Contains(read, "updated_at") {
		t.Fatalf("不该用 updated_at：\n%s", read)
	}
}

func TestRequeueRepeatTaskResubmits(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	row.Status = models.TaskStatusSuccess

	if !e.requeueRepeatTask(row) {
		t.Fatal("已注册的阶段应当重投成功")
	}
	if got := e.repeatRepolledCount.Load(); got != 1 {
		t.Fatalf("轮询重投计数 = %d, want 1", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("应入队 1 条，实得 %d", main+urgent)
	}
	// 轮询代数要 +1（后台「轮询次数」列看的就是它）
	if !strings.Contains(f.written(), "`repeat`") {
		t.Fatalf("轮询任务重置时应把 repeat 次数 +1：\n%s", f.written())
	}
}

func TestRequeueRepeatTaskSkipsUnregisteredStage(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	row := actionRow(3, "ghost")
	if e.requeueRepeatTask(row) {
		t.Fatal("未注册的阶段应当跳过")
	}
	if got := e.repeatRepolledCount.Load(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
}

func TestRepollRepeatableTasksProcessesAll(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 2, "stub", models.TaskStatusSuccess)
	pool := workerpool.NewWorkerPool[*Task](1, 16, 1)
	e := queueEngine(t, f, pool)
	e.cfg.RepeatQueue = config.RepeatQueueConfig{BatchSize: 10, WorkerCount: 2}

	n, err := e.RepollRepeatableTasks()
	if err != nil {
		t.Fatalf("RepollRepeatableTasks = %v", err)
	}
	if n != 2 {
		t.Fatalf("应重投 2 条，实得 %d", n)
	}
	st := e.GetQueueStats()[QueueRepeat]
	if st.Runs != 1 || st.LastProcessed != 2 {
		t.Fatalf("队列快照 = %+v", st)
	}
}

/* ---------- 队列开关随配置走 ---------- */

// 监控页上的 enabled 读的是**生效配置**（含运行期覆盖），
// 否则改完 interval 页面还是显示旧状态。
func TestQueueEnabledFollowsRuntimeOverride(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cfg.ErrorQueue = config.ErrorQueueConfig{Enabled: false}
	e.cfg.RepeatQueue = config.RepeatQueueConfig{Enabled: false}

	on, off := true, false
	if err := e.ApplyRuntimeConfig(&config.RuntimeConfig{
		ErrorQueue:  config.RuntimeErrorQueueConfig{Enabled: &on},
		RepeatQueue: config.RuntimeRepeatQueueConfig{Enabled: &off},
	}); err != nil {
		t.Fatal(err)
	}

	stats := e.GetQueueStats()
	if !stats[QueueError].Enabled {
		t.Fatal("error_queue 的运行期覆盖没生效")
	}
	if stats[QueueRepeat].Enabled {
		t.Fatal("repeat_queue 应保持关闭")
	}
}
