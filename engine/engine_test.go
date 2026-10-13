package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// 加急是一等字段：submitToStage 是唯一的投递入口，四条投递路径（正常提交 / 重提交 /
// 后台重投 / 错误队列重投）都走它，所以路由只在这一个地方判。
// 池子本身不认识优先级（Tasker 接口只有 Unique），队列深度是唯一可观测的证据。
func TestSubmitToRoutesByUrgentFlag(t *testing.T) {
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := &Engine{stages: map[string]*stageInfo{"s:s": {stage: "s", config: StageConfig{Site: "s"}}}, pools: newPoolRegistry()}
	e.pools.set("s:s", pool)

	if err := e.submitToStage(&Task{URL: "a", Stage: "s", Site: "s"}); err != nil {
		t.Fatalf("普通任务投递失败：%v", err)
	}
	if main, urgent := pool.QueueDepths(); main != 1 || urgent != 0 {
		t.Fatalf("普通任务应进常规队列，实得 main/urgent = %d/%d", main, urgent)
	}

	if err := e.submitToStage(&Task{URL: "b", Stage: "s", Site: "s", Urgent: true}); err != nil {
		t.Fatalf("加急任务投递失败：%v", err)
	}
	if main, urgent := pool.QueueDepths(); main != 1 || urgent != 1 {
		t.Fatalf("加急任务应进快车道，实得 main/urgent = %d/%d", main, urgent)
	}
}

// 批量插入失败要退到逐条：一行冲突不能让整批（可能上万条）一条都进不去 ——
// 而且改之前 SubmitTasks 会直接返回错误、连队列都没进，业务看到的是一整批任务凭空消失。
func TestInsertTasksFallsBackToPerRowOnBatchFailure(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	tasks := []*Task{
		{URL: "https://example.com/a", Stage: "stub"},
		{URL: "https://example.com/b", Stage: "stub"},
	}
	f.failNext = 1 // 只让那条多行 INSERT 失败

	conflicted, err := e.insertTasks(tasks)
	if err != nil {
		t.Fatalf("批量失败应退到逐条，而不是整个报错：%v", err)
	}
	if len(conflicted) != 0 {
		t.Fatalf("逐条都插成功时不该有冲突：%v", conflicted)
	}
	for i, task := range tasks {
		if task.ID == 0 {
			t.Fatalf("第 %d 条没回填 ID", i)
		}
	}
	if n := strings.Count(f.written(), "INSERT INTO"); n != 3 {
		t.Fatalf("应为 1 条批量 + 2 条逐条，实得 %d 条：\n%s", n, f.written())
	}
}

// 逐条时真撞上唯一索引（并发的另一路已经写进去了）：回填库里那行的 ID，并报成"冲突"，
// 好让调用方别重复入队 —— 与 SubmitTask 的处理一致。
func TestInsertTasksReportsConflictingRow(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	tasks := []*Task{
		{URL: "https://example.com/dup", Stage: "stub", Site: "stub"},
		{URL: "https://example.com/ok", Stage: "stub", Site: "stub"},
	}
	// 批量 INSERT 失败 + 第一条逐条 INSERT 也失败（假库里 query 恒返回 id=7 那行 → 视为"已存在"）
	f.failNext = 2

	conflicted, err := e.insertTasks(tasks)
	if err != nil {
		t.Fatalf("撞车应当被跳过，而不是报错：%v", err)
	}
	if _, hit := conflicted[tasks[0].Unique()]; !hit {
		t.Fatalf("第 1 条应被记为冲突：%v", conflicted)
	}
	if tasks[0].ID != 7 {
		t.Fatalf("冲突那条应回填库里已有行的 ID（假库里是 7），实得 %d", tasks[0].ID)
	}
	if tasks[1].ID == 0 {
		t.Fatal("没撞车的那条应当正常插入并回填 ID")
	}

	// 回查必须按 (url_hash, stage, site) —— 唯一索引就是这个（阶段名只在本站内唯一）。
	// 不能复用 findTaskRecord：它优先按 IdempotencyKey 查，而那列不是唯一索引，可能捞回另一行。
	read := f.readSQL()
	for _, want := range []string{"url_hash = ?", "stage = ?", "site = ?"} {
		if !strings.Contains(read, want) {
			t.Fatalf("回查应带 %s 条件：\n%s", want, read)
		}
	}
}

// 排空成功时 drained=true、stats 为空 —— 调用方据此照常关库、关浏览器池。
func TestEngineStopReportsDrained(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	pool.Start(context.Background(), func(context.Context, *Task) error { return nil })

	drained, stats := e.Stop(5 * time.Second)
	if !drained {
		t.Fatalf("空队列应当排空，stats = %+v", stats)
	}
	if len(stats) != 0 {
		t.Fatalf("排空时不该有存留阶段：%+v", stats)
	}
}

// 超时未排空时，必须报出**是哪个阶段**、还有多少条没跑完：
// app.Run 拿 drained=false 跳过 sqlDB.Close()，靠的就是这个信号；报错了就等于又把库关了。
func TestEngineStopReportsUnfinishedStage(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)
	e.ctx, e.cancel = context.WithCancel(context.Background())

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	pool.Start(context.Background(), func(context.Context, *Task) error {
		started <- struct{}{}
		<-release
		return nil
	})
	if err := pool.Submit(&Task{ID: 7, URL: "https://example.com", Stage: "stub"}); err != nil {
		t.Fatalf("submit = %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker 没有取到任务")
	}

	drained, stats := e.Stop(50 * time.Millisecond)
	if drained {
		t.Fatal("handler 还卡着，不该报 drained")
	}
	got, ok := stats["stub:stub"]
	if !ok {
		t.Fatalf("应当报出卡住的阶段 stub:stub，实得 %+v", stats)
	}
	if got.Unfinished != 1 {
		t.Fatalf("stub.Unfinished = %d, want 1（%+v）", got.Unfinished, got)
	}

	close(release)
}

// Stop 之后不再懒建池：晚到的投递按「先寄放」处理（行保持 pending），
// 不会出现「Stop 已返回 drained=true、新池的 worker 还在跑」那种状态。
//
// 这条窗口是框架自己的收尾协程踩出来的：Engine.Stop 先 e.cancel()，而延迟投递的
// deliverDelayed 与溢出回灌的 drainSpilled 都在 ctx 上 select —— 被 cancel 唤醒的那一次
// 照样会把手里这条投出去，于是快照之后又建出一个没人停的池。
func TestEngineStopRefusesPoolCreationAfterwards(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.pools = newPoolRegistry() // 该阶段处于「首次提交前」：登记表里没有活池
	e.ctx, e.cancel = context.WithCancel(context.Background())

	drained, stats := e.Stop(50 * time.Millisecond)
	if !drained || len(stats) != 0 {
		t.Fatalf("没有活池时应当 drained=true，实得 %v %+v", drained, stats)
	}

	// Stop 已经返回（调用方此刻会去关库/关浏览器池）—— 这一次投递不许再建池、不许起 worker
	err := e.SubmitTask(&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com"})
	if err != nil {
		t.Fatalf("停机后的投递按「先寄放」处理，不该报错：%v", err)
	}
	if pool := e.pools.get("stub:stub"); pool != nil {
		t.Fatalf("Stop 之后不该再建池，实得 %v", pool)
	}
	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("任务该留在溢出表等下次启动恢复，实得 %d", got)
	}
	if got := e.spilledCount.Load(); got != 1 {
		t.Fatalf("寄放要计进 queue_spilled（与暂停同一条路），实得 %d", got)
	}
}
