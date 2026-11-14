package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// queueEngine 在 submitEngine 之上补齐治理队列需要的那几样：
// 运行快照表、累计计数指针、以及可取消的 ctx。
func queueEngine(t *testing.T, f *fakeTaskDB, pool *workerpool.WorkerPool[*Task]) *Engine {
	t.Helper()
	e := submitEngine(t, f, pool)
	e.queueRuns = newQueueRuns()
	e.errorQueue = &errorQueueState{}
	e.repeatQueue = &repeatQueueState{wake: make(chan struct{}, 1)}
	e.queueCounters = map[string]*atomic.Int64{
		QueueError:  &e.errorQueue.retried,
		QueueRepeat: &e.repeatQueue.repolled,
	}

	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	return e
}

// setRows 把假库切成多行模式，造 n 行指定阶段/状态的任务。
// actionRow 造一条待治理队列处理的任务行。
func actionRow(id uint, stage string) *models.CrawlerTask {
	return &models.CrawlerTask{
		ID: id, Stage: stage, Site: "stub", URL: fmt.Sprintf("https://example.com/%d", id),
		Status: models.TaskStatusFailed,
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
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))
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
	e2 := queueEngine(t, f2, workerpool.NewWorkerPool[*Task](1, 8))
	e2.errorQueueQuery()().Find(&[]models.CrawlerTask{})
	if strings.Contains(f2.readSQL(), "reprocess < ?") {
		t.Fatalf("max_retry=0 表示不限，不该有 reprocess 条件：\n%s", f2.readSQL())
	}
}

// 一整批失败任务被重新投递：状态重置为 pending、代数 +1、计数累加。
func TestProcessErrorQueueRequeuesFailedTasks(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 3, "stub", models.TaskStatusFailed)
	pool := workerpool.NewWorkerPool[*Task](1, 16)
	e := queueEngine(t, f, pool)
	e.cfg.ErrorQueue = config.ErrorQueueConfig{BatchSize: 10, WorkerCount: 2}

	n, err := e.ProcessErrorQueue()
	if err != nil {
		t.Fatalf("ProcessErrorQueue = %v", err)
	}
	if n != 3 {
		t.Fatalf("应重投 3 条，实得 %d", n)
	}
	if got := e.errorQueue.retried.Load(); got != 3 {
		t.Fatalf("累计重投计数 = %d, want 3", got)
	}
	for _, r := range f.rows {
		r["status"] = int64(models.TaskStatusPending)
	}
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
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
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(3, "ghost")
	if e.requeueFailedTask()(row) {
		t.Fatal("未注册的阶段应当跳过")
	}
	if got := f.written(); got != "" {
		t.Fatalf("跳过的行不该被改写：\n%s", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("跳过的行不该入队")
	}
}

// 阶段名注册过、但**属于另一个站**的行也要跳过：键是 `site:stage`，光看名字会把它当成
// 本站那个阶段、投到错误的池子里（两个站各有 catalog 时这是最容易踩的一种）。
func TestRequeueFailedTaskSkipsRowFromAnotherSite(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool) // 夹具里注册的是 stub:stub

	row := actionRow(4, "stub")
	row.Site = "other" // 名字对得上，站点对不上

	if e.requeueFailedTask()(row) {
		t.Fatal("站点对不上的行应当跳过（阶段名只在本站内唯一）")
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
func TestRequeueFailedTaskLeavesPendingForDurableRecovery(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	pool.Stop(0)
	e := queueEngine(t, f, pool)
	if !e.requeueFailedTask()(actionRow(3, "stub")) {
		t.Fatal("persisted reset should be accepted independently of pool")
	}
	if e.errorQueue.retried.Load() != 1 || len(e.deliveryState().Notifications(stubKey)) != 1 {
		t.Fatal("reset lost recovery notification")
	}
	if strings.Contains(f.written(), "CONCAT") {
		t.Fatal("pool availability must not mark task failed")
	}
}

func TestRecoverQueueQueryShape(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))
	e.recoverQueueQuery()().Find(&[]models.CrawlerTask{})
	if sql := f.readSQL(); !strings.Contains(sql, "status =") || strings.Contains(sql, "updated_at") {
		t.Fatalf("recovery must use durable pending state, got %s", sql)
	}
}

func TestStartupProcessingResetPrecedesPendingDelivery(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)
	if err := e.preparePendingRecovery(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.written(), "WHERE status = ?") {
		t.Fatal("startup reset must be conditional on processing")
	}
	row := actionRow(7, "stub")
	row.Status = models.TaskStatusPending
	if !e.requeueRecoverTask(row) || queued(t, pool) != 1 {
		t.Fatal("reset pending task was not delivered")
	}
}

func TestRequeueRecoverTaskClearsDedupCache(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	row.Status = models.TaskStatusPending
	key := (&Task{Stage: "stub", Site: row.Site, URL: row.URL}).Unique()
	e.dedupCache.Add(key)

	if !e.requeueRecoverTask(row) {
		t.Fatal("剔除缓存后应当能恢复成功")
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("应入队 1 条，实得 %d（被 dedupCache 挡住就是 0）", main+urgent)
	}
}

// 直接放回内存队列。这是启动恢复最常见的形态：再重置/再写 pending 只会刷新 updated_at。
func TestRequeueRecoverTaskPendingRowSkipsDB(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	row.Status = models.TaskStatusPending

	if !e.requeueRecoverTask(row) {
		t.Fatal("已注册且仍 pending 的行应当恢复成功")
	}
	if got := e.recoveredCount.Load(); got != 1 {
		t.Fatalf("恢复计数 = %d, want 1", got)
	}
	if got := f.written(); got != "" {
		t.Fatalf("行已是 pending，不该再写库：\n%s", got)
	}
	if got := f.readSQL(); got != "" {
		t.Fatalf("不该再走 SubmitTask 的去重查库：\n%s", got)
	}
	if main, urgent := pool.QueueDepths(); main != 1 || urgent != 0 {
		t.Fatalf("普通 pending 恢复应保留原优先级，实得 main/urgent = %d/%d", main, urgent)
	}
}

func TestRequeueRecoverTaskSkipsUnregisteredStage(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(3, "ghost")
	if e.requeueRecoverTask(row) {
		t.Fatal("未注册的阶段应当跳过")
	}
	if got := e.recoveredCount.Load(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
}

// 启动恢复最常见的一批是「上次暂停或中断投递留下的 pending」：这类行状态本来就对，
// 恢复只该把它们放回内存队列，**一条 SQL 的写都不该有**（这是启动恢复从 ~500 条 SQL 降下来的主因）。
func TestProcessRecoverQueuePendingRowsAreReadOnly(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 120, "stub", models.TaskStatusPending)
	pool := workerpool.NewWorkerPool[*Task](1, 256)
	e := queueEngine(t, f, pool)
	if _, err := e.ProcessRecoverQueue(); err != nil {
		t.Fatal(err)
	}
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
	}
	if f.written() != "" || queued(t, pool) != 120 {
		t.Fatalf("pending recovery writes=%s queued=%d", f.written(), queued(t, pool))
	}
}

func TestProcessRecoverQueueDoesNotResetLiveProcessing(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 2, "stub", models.TaskStatusProcessing)
	pool := workerpool.NewWorkerPool[*Task](1, 16)
	e := queueEngine(t, f, pool)
	if _, err := e.ProcessRecoverQueue(); err != nil {
		t.Fatal(err)
	}
	if err := e.recoverPendingStage(stubKey); err != nil {
		t.Fatal(err)
	}
	if f.written() != "" || queued(t, pool) != 0 {
		t.Fatal("runtime recovery reset or duplicated processing tasks")
	}
}

func TestPendingRecoveryRemainsAvailableWithDefaultConfig(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)
	e.cfg.RecoverQueue = config.RecoverQueueConfig{}
	e.startRecoverQueue()
	waitQueueDepth(t, pool, 1)
	e.cancel()
	e.tickerWG.Wait()
}

func TestRepeatQueueQueryShape(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))

	e.repeatQueueQuery()().Find(&[]models.CrawlerTask{})
	read := f.readSQL()
	if !strings.Contains(read, "repeatable = ?") {
		t.Fatalf("应只捞 repeatable：\n%s", read)
	}
	if !strings.Contains(read, "status IN") {
		t.Fatalf("应限定 success/failed：\n%s", read)
	}
	// 判据是 next_repeat_at（到点才捞）。这条只钉 SQL 形态 —— 假库不按 WHERE 过滤，
	// 真过滤在 MySQL 那边，能证它的是"重置语句写 next_repeat_at + 复投前再确认一次"那两条。
	if !strings.Contains(read, "next_repeat_at") {
		t.Fatalf("应按下次到点时间过滤：\n%s", read)
	}
	if strings.Contains(read, "updated_at") {
		t.Fatalf("不该用 updated_at（它被每次写都刷，算不出周期）：\n%s", read)
	}
}

func TestRequeueRepeatTaskResubmits(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	row.Status = models.TaskStatusSuccess

	if !e.requeueRepeatTask()(row) {
		t.Fatal("已注册的阶段应当重投成功")
	}
	if got := e.repeatRepolledTotal(); got != 1 {
		t.Fatalf("轮询重投计数 = %d, want 1", got)
	}
	if got := len(e.deliveryState().Notifications(stubKey)); got != 1 {
		t.Fatal("repeat reset must signal durable recovery")
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("重置阶段不直接阻塞投递，实得 %d", main+urgent)
	}
	// 轮询代数要 +1（后台「轮询次数」列看的就是它）
	if !strings.Contains(f.written(), "`repeat`") {
		t.Fatalf("轮询任务重置时应把 repeat 次数 +1：\n%s", f.written())
	}
	// 刚做过条件重置，不该再走 SubmitTask 的去重查库
	if got := f.readSQL(); got != "" {
		t.Fatalf("轮询重投不该再查一次去重表：\n%s", got)
	}
}

func TestRequeueRepeatTaskSkipsUnregisteredStage(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(3, "ghost")
	if e.requeueRepeatTask()(row) {
		t.Fatal("未注册的阶段应当跳过")
	}
	if got := e.repeatRepolledTotal(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
}

// 重投前的条件重置：守卫与三个字段都写在语句里（假库不认 WHERE，只能钉 SQL 形态）。
// 少了它，批次 SELECT 之后运营点的「停轮询」会被这一轮重投盖过去。
func TestRepeatResetScopeCarriesConditions(t *testing.T) {
	db := dryDB(t)
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return repeatResetScope(tx, 7).Updates(map[string]any{
			"status":         models.TaskStatusPending,
			"last_repeat_at": gorm.Expr("NOW()"),
			"next_repeat_at": gorm.Expr(
				"FROM_UNIXTIME(UNIX_TIMESTAMP(NOW()) + COALESCE(NULLIF(repeat_interval, 0), ?))", int64(7200)),
		})
	})
	for _, want := range []string{
		"UPDATE", "id = 7", "repeatable = 1", "status IN",
		"`status`=0",                                                  // 重置为待处理
		"`last_repeat_at`=NOW()",                                      // 记录：上次轮询时刻
		"FROM_UNIXTIME", "COALESCE(NULLIF(repeat_interval, 0), 7200)", // 判据：下次到点（0 = 跟全局 7200）
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("SQL 里缺少 %q：\n%s", want, sql)
		}
	}
}

// 节拍跟随"最早到点"：假库的 MIN 查询返回值就是"还差几秒"（真过滤在 SQL 层，这里验换算与钳位）。
func TestRepeatTickIntervalFollowsSoonestDue(t *testing.T) {
	epochIn := func(d time.Duration) *int64 {
		v := time.Now().Add(d).Unix()
		return &v
	}
	cases := []struct {
		name   string
		global time.Duration
		min    *int64 // 假库 MIN 的返回值；nil = NULL（没有可轮询的行）
		failQ  bool
		want   time.Duration
	}{
		{"全局 interval = 0 使用默认", 0, nil, false, 12 * time.Hour},
		{"没有可轮询的行 → 全局节拍", time.Hour, nil, false, time.Hour},
		{"查库出错 → 全局节拍（库抖动别把队列搞停）", time.Hour, nil, true, time.Hour},
		{"30 秒后到点 → 跟随它（比全局细）", time.Hour, epochIn(30 * time.Second), false, 30 * time.Second},
		{"已经到点 → 钳到最短刻度（别空转）", time.Hour, epochIn(-time.Minute), false, repeatMinTick},
		{"下一个到点比全局还远 → 用全局（最粗兜底）", time.Minute, epochIn(time.Hour), false, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.minEpoch = tc.min
			if tc.failQ {
				f.failQueries = 1
			}
			e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))
			e.cfg.RepeatQueue = config.RepeatQueueConfig{Interval: tc.global}

			got := e.repeatTickInterval()
			if diff := got - tc.want; diff > time.Second || diff < -time.Second {
				t.Fatalf("repeatTickInterval = %v, want %v", got, tc.want)
			}
		})
	}
}

// 行在批次 SELECT 之后已经不该被重投了（刚被「停轮询」，或已被「重投」/「删除」接手）：
// 条件重置 0 行 → 跳过，不计数、不入队、也不标 failed —— 它不是"重投失败"。
func TestRequeueRepeatTaskSkipsWhenStopped(t *testing.T) {
	f := newFakeTaskDB()
	f.affected = 0 // 重置匹配不到行
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	if e.requeueRepeatTask()(actionRow(7, "stub")) {
		t.Fatal("行已不再可轮询时应跳过")
	}
	if got := e.repeatRepolledTotal(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("跳过的不该入队，实得 %d", main+urgent)
	}
	if sql := f.written(); !strings.Contains(sql, "repeatable = ?") {
		t.Fatalf("重置语句里应带 repeatable 守卫：\n%s", sql)
	}
}

// 运行期开关驱动的就是队列判断的那一列。假库既不按 WHERE 过滤、也不真执行 UPDATE
// （单行模式恒返回那一行），所以"开着就会被捞到、停掉就不会"只能靠两条证据钉住：
// ① 查询条件里必须有 repeatable（真正的过滤在 SQL 层）；
// ② 重投前的条件重置在"已不可轮询"时 0 行 → 跳过。
func TestRepeatableFlagDrivesPolling(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 16)
	e := queueEngine(t, f, pool)
	e.cfg.RepeatQueue = config.RepeatQueueConfig{BatchSize: 10, WorkerCount: 1}

	// ① 队列只捞 repeatable = 1 的终态行
	e.repeatQueueQuery()().Find(&[]models.CrawlerTask{})
	if read := f.readSQL(); !strings.Contains(read, "repeatable = ?") || !strings.Contains(read, "status IN") {
		t.Fatalf("轮询查询应按 repeatable + 终态过滤：\n%s", read)
	}

	// ② 「开轮询」只改这一列
	if err := e.SetTaskRepeatable(7, true); err != nil {
		t.Fatalf("SetTaskRepeatable(7, true) = %v", err)
	}
	if !strings.Contains(f.written(), "`repeatable`") {
		t.Fatalf("开关应写 repeatable 列：\n%s", f.written())
	}
	// 假库不真按 SQL 改行，手工对齐"这一列已经落库"
	f.row["repeatable"] = int64(models.RepeatableYes)

	// 开着：下一轮扫到它并重投（假库那一行的 status 默认就是待处理，重投路径照常走）
	if n, err := e.RepollRepeatableTasks(); err != nil || n != 1 {
		t.Fatalf("RepollRepeatableTasks = %d, %v, want 1, nil", n, err)
	}

	// ③ 「停轮询」之后：重置匹配不到这一行 → 这一轮不再投它
	if err := e.SetTaskRepeatable(7, false); err != nil {
		t.Fatalf("SetTaskRepeatable(7, false) = %v", err)
	}
	f.mu.Lock()
	f.affected = 0
	f.row["repeatable"] = int64(models.RepeatableNo)
	f.mu.Unlock()

	if e.requeueRepeatTask()(actionRow(7, "stub")) {
		t.Fatal("停掉的轮询任务不该再被重投")
	}
	if got := e.repeatRepolledTotal(); got != 1 {
		t.Fatalf("轮询重投计数 = %d, want 1（停掉的那次不该计数）", got)
	}
}

func TestRepollRepeatableTasksProcessesAll(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 2, "stub", models.TaskStatusSuccess)
	pool := workerpool.NewWorkerPool[*Task](1, 16)
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

/* ---------- 治理队列状态守卫与关停 ---------- */

// 批次 SELECT 与这次重置之间有窗口：那一行可能已经被删/标失败/自己跑完了。
// 重置必须**带条件**并查 RowsAffected —— 否则 0 行时后面 SubmitTask 会走 Insert
// 把删掉的任务重新插回来，或把已终态的行翻回 pending 再跑一遍。
func TestRequeueRecoverTaskSkipsChangedRows(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(7, "stub")
	row.Status = models.TaskStatusProcessing

	// 模拟"条件没匹配上"：这一行已经不该被恢复了
	f.mu.Lock()
	f.affected = 0
	f.mu.Unlock()

	if e.requeueRecoverTask(row) {
		t.Fatal("条件重置 0 行时应当跳过（这一行已经不该被恢复），不该报成功")
	}
	if got := e.recoveredCount.Load(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("跳过的不该入队，实得 %d", main+urgent)
	}
	if f.written() != "" {
		t.Fatal("runtime recovery must never reset live processing rows")
	}
}

func TestRequeueFailedTaskSkipsRowsNoLongerFailed(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := queueEngine(t, f, pool)

	row := actionRow(5, "stub")
	row.Status = models.TaskStatusFailed

	// 守卫的形状：语句里必须有 status 判据（行已经不再 failed 时才跳过）
	e.requeueFailedTask()(row)
	sql := f.written()
	if !strings.Contains(sql, "status = ?") {
		t.Fatalf("重置语句里应当有 status 守卫：%s", sql)
	}

	// 行为：条件没匹配上（轮询队列刚把它捞走了）→ 跳过，不计数也不投递
	f2 := newFakeTaskDB()
	pool2 := workerpool.NewWorkerPool[*Task](1, 8)
	e2 := queueEngine(t, f2, pool2)
	f2.mu.Lock()
	f2.affected = 0
	f2.mu.Unlock()
	if e2.requeueFailedTask()(row) {
		t.Fatal("行已经不再是 failed（被轮询队列捞走）时应跳过")
	}
	if got := e2.errorQueue.retried.Load(); got != 0 {
		t.Fatalf("跳过的不该计数，实得 %d", got)
	}
	if main, urgent := pool2.QueueDepths(); main+urgent != 0 {
		t.Fatalf("跳过的不该入队，实得 %d", main+urgent)
	}
}

// Stop 必须等待阶段恢复等后台循环结束，不能仅因 worker 空闲就报告排空。
func TestStopWaitsForBackgroundLoops(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))
	// worker 要真起：池子里压着没跑完的任务时 Stop 不该报"排空"（那是另一条契约）
	e.pools.get("stub:stub").Start(e.ctx, func(context.Context, *Task) error { return nil })
	e.requestPendingRecovery(&Task{Stage: "stub", Site: "stub"})
	e.startPendingRecovery()

	time.Sleep(80 * time.Millisecond) // 至少跑过一轮
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("Stop 必须等待持久化恢复协程退出")
	}
	before := f.written()
	time.Sleep(120 * time.Millisecond)
	if after := f.written(); after != before {
		t.Fatalf("Stop 返回后不该再有人写库：\n新增：\n%s", strings.TrimPrefix(after, before))
	}
}

// 启动恢复的裸 goroutine 也要被 Stop 的「排空」覆盖：恢复中途写库（批量 SELECT / 重置 / 提交），
// 不纳入 tickerWG 的话 Stop 会在它还查库时返回 drained=true，app.Run 随即关库。
// 这里用 onQuery 把恢复卡在第一次 SELECT 上：Stop 必须等它退出才返回（不修的话 Stop 会立即返回）。
func TestStopWaitsForStartupRecover(t *testing.T) {
	f := newFakeTaskDB()
	setRows(f, 1, "stub", models.TaskStatusProcessing)
	pool := workerpool.NewWorkerPool[*Task](1, 16)
	e := queueEngine(t, f, pool)
	e.cfg.RecoverQueue = config.RecoverQueueConfig{BatchSize: 10}

	release := make(chan struct{})
	entered := make(chan struct{})
	f.onQuery = func(n int) {
		if n == 1 {
			close(entered)
			<-release
		}
	}

	e.startRecoverQueue()
	<-entered

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.Stop(time.Second)
	}()

	// 恢复还卡着，Stop 不该返回（否则它把这跳写库当不存在）
	select {
	case <-stopped:
		t.Fatal("恢复还没结束，Stop 不该返回")
	case <-time.After(50 * time.Millisecond):
	}

	close(release) // 放行恢复

	select {
	case <-stopped:
		// Stop 等到恢复退出才返回
	case <-time.After(time.Second):
		t.Fatal("放行后 Stop 应当等到恢复退出")
	}
}
