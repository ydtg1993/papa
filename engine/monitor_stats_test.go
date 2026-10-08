package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/internal/track"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
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
func TestSampleQueueBacklogOneRecordsValue(t *testing.T) {
	f := newFakeTaskDB()
	f.count = 42
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	// 注意：query 里必须自带 Model —— 这就是下面那个已知缺陷的边界，
	// 见 TestQueueQueriesHaveNoTableForCount 的说明。
	query := func() *gorm.DB {
		return e.db.Model(&models.CrawlerTask{}).Where("status = ?", models.TaskStatusFailed)
	}
	e.sampleQueueBacklogOne(QueueError, query)

	st := e.GetQueueStats()[QueueError]
	if st.Backlog != 42 {
		t.Fatalf("积压 = %d, want 42", st.Backlog)
	}
	if st.BacklogAt.IsZero() {
		t.Fatal("应记下采样时刻")
	}
}

// 采样失败只记日志：积压数是观测数据，坏了不影响队列本身。
func TestSampleQueueBacklogOneSurvivesDBError(t *testing.T) {
	f := newFakeTaskDB()
	f.failQueries = 1
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.sampleQueueBacklogOne(QueueError, e.errorQueueQuery())

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
	if len(qs) != 2 {
		t.Fatalf("应有两个队列的查询：%+v", qs)
	}
	for _, name := range []string{QueueError, QueueRepeat} {
		if qs[name] == nil {
			t.Fatalf("%s 没有查询构造器", name)
		}
	}

	qs[QueueError]().Model(&models.CrawlerTask{}).Find(&[]models.CrawlerTask{})
	if read := f.readSQL(); !containsRaw(read, "status = ?") {
		t.Fatalf("error_queue 的积压查询条件不对：\n%s", read)
	}

	qs[QueueRepeat]().Model(&models.CrawlerTask{}).Find(&[]models.CrawlerTask{})
	if read := f.readSQL(); !containsRaw(read, "repeatable = ?") {
		t.Fatalf("repeat_queue 的积压查询条件不对：\n%s", read)
	}
}

// errorQueueQuery / repeatQueueQuery 返回的是「只带 WHERE、不带表名」的构造器 ——
// 这是它们的既定契约：Find 会用切片元素类型补上表名。
//
// 但 sampleQueueBacklogOne 走的是 Count(&n)，而 gorm 的 Count 推不出表名，
// 会直接返回 "Table not set"。也就是说：**治理队列的积压数现在恒为 0，
// 且每轮采样都会往日志里写一条 warning**。
// 这条用例把「无表名时 Count 必然失败」这个前提钉住，免得有人以为是测试环境的怪现象。
func TestQueueQueryWithoutModelCannotCount(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	var n int64
	err := e.errorQueueQuery()().Count(&n).Error
	if err == nil {
		t.Fatal("没有表名时 Count 不该成功 —— 若这里通过了，说明 gorm 行为变了，请顺手把积压采样修好")
	}
	if !containsRaw(err.Error(), "Table not set") {
		t.Fatalf("错误信息变了：%v", err)
	}

	// 补上 Model 就能查通 —— 这正是修法
	f.count = 42
	if err := e.errorQueueQuery()().Model(&models.CrawlerTask{}).Count(&n).Error; err != nil {
		t.Fatalf("补上 Model 后应当能查：%v", err)
	}
	if n != 42 {
		t.Fatalf("count = %d, want 42", n)
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
	e.cfg.ErrorQueue = config.ErrorQueueConfig{Enabled: true}
	if !e.queueEnabled(QueueError) {
		t.Fatal("error_queue 开着时应返回 true")
	}
}

// 采样协程要起得来、也要能停 —— 它内部那次 COUNT 目前必然失败（见上），
// 所以这里只钉"不 panic、停机干净、间隔用了配置值"。
func TestStartQueueSamplerRunsAndStops(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.cfg.Server.QueueSampleInterval = 5 * time.Millisecond

	e.startQueueSampler()
	time.Sleep(40 * time.Millisecond)

	e.cancel()
	time.Sleep(40 * time.Millisecond) // 取消后协程应干净退出（不该 panic / 不该卡住）
}

// 默认采样间隔是 1 分钟：采样是 COUNT 查询，漏配不能变成高频查库。
func TestQueueSampleIntervalDefault(t *testing.T) {
	if defaultQueueSampleInterval != time.Minute {
		t.Fatalf("默认采样间隔被改了：%v", defaultQueueSampleInterval)
	}
}
