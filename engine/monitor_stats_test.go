package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

/* ---------- 阶段统计快照 ---------- */

// setStatsQueue 懒建 map；GetStageStats 把内部结构翻成**纯值**快照交出去
// （监控页只该读值，不该摸到内部实现）。
func TestGetStageStatsSnapshot(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stats := track.NewStatsQueue[*Task](pool)
	stats.Start(ctx)
	e.setStatsQueue("stub", stats)

	pool.Start(ctx, func(_ context.Context, _ *Task) error { return nil })
	defer pool.Stop(time.Second)
	if err := pool.Submit(&Task{ID: 7, Stage: "stub", URL: "https://example.com"}); err != nil {
		t.Fatalf("Submit = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if e.GetStageStats()["stub"].Global.TotalTasks == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("统计没跟上：%+v", e.GetStageStats())
		}
		time.Sleep(5 * time.Millisecond)
	}

	snap, ok := e.GetStageStats()["stub"]
	if !ok {
		t.Fatal("快照里应有 stub 阶段")
	}
	if snap.Global.TotalTasks != 1 || snap.Global.TotalFailed != 0 {
		t.Fatalf("全局统计 = %+v", snap.Global)
	}
	if len(snap.Workers) != 1 {
		t.Fatalf("应有一个 worker 的统计，实得 %+v", snap.Workers)
	}
	w, ok := snap.Workers[0]
	if !ok {
		t.Fatalf("worker 0 的统计丢了：%+v", snap.Workers)
	}
	if w.TotalTasks != 1 || w.WorkerID != 0 {
		t.Fatalf("worker 统计 = %+v", w)
	}
	// 队列计数来自池子的原子计数
	if snap.Queue.Submitted != 1 || snap.Queue.Completed != 1 {
		t.Fatalf("队列计数 = %+v", snap.Queue)
	}
	if snap.Queue.InProgress != 0 || snap.Queue.QueueLen != 0 {
		t.Fatalf("跑完之后不该有在途/排队：%+v", snap.Queue)
	}
}

// 没注册任何阶段时返回空 map（不是 nil）—— 监控页直接 range 它。
func TestGetStageStatsEmpty(t *testing.T) {
	e := &Engine{}
	got := e.GetStageStats()
	if got == nil {
		t.Fatal("应返回空 map 而不是 nil")
	}
	if len(got) != 0 {
		t.Fatalf("没有阶段时不该有条目：%+v", got)
	}
}

/* ---------- 积压采样 ---------- */

// 采样结果落到快照上，并记下采样时刻（监控页据此判断这个数字新不新）。
//
// query 用的是**真的** errorQueueQuery（不是手搭一个带 Model 的替身）——
// 这一步就是这条链路的回归覆盖：构造器少带 Model 的话，这里的 Count 会直接报错、积压留成 0。
func TestSampleQueueBacklogOneRecordsValue(t *testing.T) {
	f := newFakeTaskDB()
	f.count = 42
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.sampleQueueBacklogOne(errorQueueKey(""), e.errorQueueQuery(""))

	st := e.GetQueueStats()[QueueError]
	if st.Backlog != 42 {
		t.Fatalf("积压 = %d, want 42", st.Backlog)
	}
	if st.BacklogAt.IsZero() {
		t.Fatal("应记下采样时刻")
	}
}

// 一轮采样覆盖两个队列，且都拿到真值。
func TestSampleQueueBacklogWritesBothQueues(t *testing.T) {
	f := newFakeTaskDB()
	f.count = 7
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.sampleQueueBacklog()

	stats := e.GetQueueStats()
	for _, name := range queueKeysForTest(e) {
		if got := stats[name].Backlog; got != 7 {
			t.Errorf("%s 的积压 = %d, want 7", name, got)
		}
		if stats[name].BacklogAt.IsZero() {
			t.Errorf("%s 没记下采样时刻", name)
		}
	}
}

// 采样失败只记日志：积压数是观测数据，坏了不影响队列本身。
func TestSampleQueueBacklogOneSurvivesDBError(t *testing.T) {
	f := newFakeTaskDB()
	f.failQueries = 1
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.sampleQueueBacklogOne(QueueError, e.errorQueueQuery(""))

	st := e.GetQueueStats()[QueueError]
	if st.Backlog != 0 {
		t.Fatalf("查失败时不该写入假的积压数，实得 %d", st.Backlog)
	}
	if !st.BacklogAt.IsZero() {
		t.Fatal("查失败时不该记采样时刻")
	}
}

// 每个队列自带一份"待处理"条件构造器：error 查 failed、repeat 查已完成的 repeatable。
//
// 注意这里必须补上 Model：这两个构造器只拼 WHERE、不带表名，
// Find 能靠切片元素类型推出表，**Count 推不出来**（见下面的说明）。
func TestQueueQueriesCoverRegisteredQueues(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	qs := e.queueQueries()
	// 错误队列与轮询队列**都按站点拆**（默认 scope 那两份永远在）
	if want := len(queueKeysForTest(e)); len(qs) != want {
		t.Fatalf("应有 %d 个队列的查询（每站错误 + 每站轮询）：%+v", want, qs)
	}
	for _, name := range queueKeysForTest(e) {
		if qs[name] == nil {
			t.Fatalf("%s 没有查询构造器", name)
		}
	}

	qs[QueueError]().Find(&[]models.CrawlerTask{})
	if read := f.readSQL(); !containsRaw(read, "status = ?") {
		t.Fatalf("error_queue 的积压查询条件不对：\n%s", read)
	}

	qs[QueueRepeat]().Find(&[]models.CrawlerTask{})
	read := f.readSQL()
	if !containsRaw(read, "repeatable = ?") || !containsRaw(read, "site = ?") {
		t.Fatalf("repeat_queue 的积压查询条件不对：\n%s", read)
	}
}

// 两个队列的积压构造器都要能**直接 Count**。
//
// 回归点：`sampleQueueBacklogOne` 走的是 `Count(&n)`，而 gorm 的 Count
// **推不出表名**（`Find(&slice)` 能，靠元素类型）。曾经这两个构造器只拼 WHERE 没带 Model，
// 结果是 —— 治理队列的积压数**恒为 0**、`backlog_at` 恒为零值，且每轮采样
// （默认 1 分钟一轮）都往 engine.log 写一条 "Table not set" 的 WARN，把真错误淹掉。
//
// 这条用例直接对构造器 Count，所以能独立于采样协程把这个契约钉住。
func TestQueueQueriesCanBeCountedDirectly(t *testing.T) {
	f := newFakeTaskDB()
	f.count = 42
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	for _, c := range []struct {
		name  string
		query func() *gorm.DB
	}{
		{QueueError, e.errorQueueQuery("")},
		{QueueRepeat, e.repeatQueueQuery("")},
	} {
		var n int64
		if err := c.query().Count(&n).Error; err != nil {
			t.Fatalf("%s 的构造器应当能直接 Count（漏了 Model 就会 Table not set）：%v", c.name, err)
		}
		if n != 42 {
			t.Fatalf("%s count = %d, want 42", c.name, n)
		}
	}

	// 用真实构造器跑一遍采样：积压数必须是查询回来的值，而不是 0
	e.sampleQueueBacklog()
	for _, name := range queueKeysForTest(e) {
		if got := e.GetQueueStats()[name].Backlog; got != 42 {
			t.Fatalf("%s 采样后的积压 = %d, want 42", name, got)
		}
	}
}

// 队列名不认识时 enabled 一律 false（新队列忘了登记，也不该在页面上显示成"开着"）。
func TestQueueEnabledUnknownName(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	if e.queueEnabled("no_such_queue") {
		t.Fatal("未知队列名应当返回 false")
	}
	if e.queueEnabled(QueueError) {
		t.Fatal("配置零值（关）时不该返回 true")
	}
	setSiteQueues(e, "", SiteQueues{Error: config.ErrorQueueConfig{Enabled: true}})
	if !e.queueEnabled(QueueError) {
		t.Fatal("error_queue 开着时应返回 true")
	}
}

// 采样协程要起得来、也要能停 —— 它内部那次 COUNT 目前必然失败（见上），
// 所以这里只钉"不 panic、停机干净、间隔用了配置值"。
// 采样协程的端到端：起来先采一轮（页面第一眼就有数），之后按配置间隔重复。
//
// 这条在修复"构造器漏了 Model"之前**必然失败** —— 那时 Count 报 Table not set，
// 积压数会永远停在 0，而这条盯的正是"后台上的数字真的是查回来的"。
func TestStartQueueSamplerSamplesImmediately(t *testing.T) {
	f := newFakeTaskDB()
	f.count = 5
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cfg.Server.QueueSampleInterval = 10 * time.Millisecond

	e.startQueueSampler()

	deadline := time.Now().Add(3 * time.Second)
	for e.GetQueueStats()[QueueError].Backlog != 5 {
		if time.Now().After(deadline) {
			t.Fatalf("采样协程没跑起来：%+v", e.GetQueueStats()[QueueError])
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, name := range queueKeysForTest(e) {
		st := e.GetQueueStats()[name]
		if st.Backlog != 5 {
			t.Errorf("%s 的积压 = %d, want 5", name, st.Backlog)
		}
		if st.BacklogAt.IsZero() {
			t.Errorf("%s 没记下采样时刻", name)
		}
	}

	// 间隔真的生效：10ms 一轮，说明按期重复采而不是只采一次
	first := e.GetQueueStats()[QueueError].BacklogAt
	deadline = time.Now().Add(3 * time.Second)
	for !e.GetQueueStats()[QueueError].BacklogAt.After(first) {
		if time.Now().After(deadline) {
			t.Fatal("采样没有按期重复")
		}
		time.Sleep(5 * time.Millisecond)
	}

	e.cancel()
	time.Sleep(40 * time.Millisecond) // 取消后协程应干净退出（不该 panic / 不该卡住）
}

// 默认采样间隔是 1 分钟：采样是 COUNT 查询，漏配不能变成高频查库。
func TestQueueSampleIntervalDefault(t *testing.T) {
	if defaultQueueSampleInterval != time.Minute {
		t.Fatalf("默认采样间隔被改了：%v", defaultQueueSampleInterval)
	}
}

// queueKeysForTest 已登记的治理队列键：错误与轮询各按站点（默认 scope 那两份永远在）。
func queueKeysForTest(e *Engine) []string {
	keys := make([]string, 0, len(e.errorQueues)+len(e.repeatQueues))
	for _, site := range e.errorQueueSites() {
		keys = append(keys, errorQueueKey(site))
	}
	for _, site := range e.repeatQueueSites() {
		keys = append(keys, repeatQueueKey(site))
	}
	return keys
}
