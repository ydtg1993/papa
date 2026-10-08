package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
)

/* ---------- 高水位溢出与回灌 ---------- */

func TestSpillBacklogCountsAcrossStages(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	if got := e.spillBacklog(); got != 0 {
		t.Fatalf("初始应为空，实得 %d", got)
	}
	e.spillTask(&Task{Stage: "stub", URL: "a"})
	e.spillTask(&Task{Stage: "stub", URL: "b"})
	e.spillTask(&Task{Stage: "other", URL: "c"})

	if got := e.spillBacklog(); got != 3 {
		t.Fatalf("溢出积压 = %d, want 3（跨阶段累加）", got)
	}
}

// 队列腾出空间后回灌：行还在待处理态就重新入队。
func TestDrainSpilledRequeuesPendingTask(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com/spilled"}
	e.spillTask(task)
	e.drainSpilled()

	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("回灌后应入队 1 条，实得 %d", main+urgent)
	}
	if got := e.spillBacklog(); got != 0 {
		t.Fatalf("回灌过的任务应离开溢出列表，实得 %d", got)
	}
}

// 行已经到终态（运营在溢出期间标了失败 / 别处跑成功了）→ 直接丢弃，不再执行。
func TestDrainSpilledDropsTerminalTask(t *testing.T) {
	f := newFakeTaskDB()
	f.row["status"] = int64(models.TaskStatusFailed)
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	e.spillTask(&Task{ID: 7, Stage: "stub", URL: "https://example.com/done"})
	e.drainSpilled()

	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("已到终态的任务不该再入队")
	}
	if got := e.spillBacklog(); got != 0 {
		t.Fatalf("终态任务应被彻底丢弃，实得积压 %d", got)
	}
}

// 行不存在（运营删了它）→ 丢弃，不该没完没了地留在溢出列表里重试。
func TestDrainSpilledDropsMissingRow(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	e.spillTask(&Task{ID: 7, Stage: "stub", URL: "https://example.com/gone"})
	e.drainSpilled()

	if got := e.spillBacklog(); got != 0 {
		t.Fatalf("行不存在应当丢弃，实得积压 %d", got)
	}
}

// 库抖动（不是"行不存在"）→ 放回溢出列表，下一轮再试。
// 分不清这两种就只能二选一：要么丢掉真任务，要么永远重试，所以这里必须分开。
func TestDrainSpilledKeepsTaskOnTransientDBError(t *testing.T) {
	f := newFakeTaskDB()
	f.failQueries = 1
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	e.spillTask(&Task{ID: 7, Stage: "stub", URL: "https://example.com/flaky"})
	e.drainSpilled()

	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("瞬态错误应把任务放回溢出列表，实得积压 %d", got)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("查不到行时不该入队")
	}
}

// 阶段没注册：整批跳过（这里只钉住"不会因此 panic 或入错队列"）。
func TestDrainSpilledSkipsUnregisteredStage(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	e.spillTask(&Task{ID: 7, Stage: "ghost", URL: "https://example.com/ghost"})
	e.drainSpilled()

	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("未注册阶段的任务不该入任何队列")
	}
	if got := e.spillBacklog(); got != 0 {
		t.Fatalf("未注册阶段整批跳过，积压清空，实得 %d", got)
	}
}

func TestDrainSpilledNoopWhenEmpty(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.drainSpilled() // 不该碰库、不该 panic
	if got := f.readSQL(); got != "" {
		t.Fatalf("空列表不该查库：\n%s", got)
	}
}

/* ---------- 延迟投递的 dispatcher ---------- */

// delayEngine 起一个延迟派发协程，返回可用的引擎。
func delayEngine(t *testing.T, f *fakeTaskDB, pool *workerpool.WorkerPool[*Task]) *Engine {
	t.Helper()
	e := submitEngine(t, f, pool)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	go e.delayDispatcher()
	return e
}

func delayRecord(id uint) models.CrawlerTask {
	return models.CrawlerTask{
		ID: id, Stage: "stub", URL: "https://example.com/delayed",
		Status: models.TaskStatusPending,
	}
}

func waitQueueDepth(t *testing.T, pool *workerpool.WorkerPool[*Task], want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		main, urgent := pool.QueueDepths()
		if main+urgent == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到队列深度 %d，实得 %d", want, main+urgent)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 到点的延迟任务立刻入队（"到点"判定用的是绝对时间，所以过去时间点=立即）。
func TestDelayDispatcherDeliversDueTask(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := delayEngine(t, f, pool)

	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", URL: "https://example.com/delayed"}, delayRecord(7))

	waitQueueDepth(t, pool, 1)
	// 入队时同样要先落库（"已入队 ⇒ 行里是 pending" 的不变量）
	if got := f.written(); !containsRaw(got, "UPDATE") {
		t.Fatalf("入队前应把行置为 pending：\n%s", got)
	}
}

// 未到点的任务不该被提前投递 —— 延迟投递的意义就是不占 worker 的并发位。
func TestDelayDispatcherHoldsFutureTask(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := delayEngine(t, f, pool)

	e.enqueueDelayed(time.Now().Add(time.Hour),
		&Task{ID: 7, Stage: "stub", URL: "https://example.com/later"}, delayRecord(7))

	time.Sleep(60 * time.Millisecond)
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("未到点不该入队，实得 %d", main+urgent)
	}

	e.delayMu.Lock()
	n := e.delayHeap.Len()
	e.delayMu.Unlock()
	if n != 1 {
		t.Fatalf("任务应留在延迟堆里，实得 %d", n)
	}
}

// 等待期间来了更早的任务：要唤醒它重新计算等待时间，否则早的任务会被晚的拖住。
func TestDelayDispatcherRecomputesOnEarlierTask(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := delayEngine(t, f, pool)

	// 先排一个 1 小时后的
	e.enqueueDelayed(time.Now().Add(time.Hour),
		&Task{ID: 7, Stage: "stub", URL: "https://example.com/later"}, delayRecord(7))
	time.Sleep(30 * time.Millisecond) // 让它进入定时器等待

	// 再来一个已经到点的
	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", URL: "https://example.com/now"}, delayRecord(7))

	waitQueueDepth(t, pool, 1)

	e.delayMu.Lock()
	n := e.delayHeap.Len()
	e.delayMu.Unlock()
	if n != 1 {
		t.Fatalf("只有到点的那条该被取走，延迟堆应剩 1 条，实得 %d", n)
	}
}

// 引擎停机 → 派发协程退出，此后不再投递任何东西。
func TestDelayDispatcherExitsOnCtxCancel(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := delayEngine(t, f, pool)

	e.cancel()
	time.Sleep(80 * time.Millisecond) // 让协程走完 ctx.Done 那一支

	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", URL: "https://example.com/dead"}, delayRecord(7))
	time.Sleep(80 * time.Millisecond)

	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("停机后不该再投递，实得 %d", main+urgent)
	}
}

// 延迟入队要主动叫醒派发协程（非阻塞信号），否则新任务得等上一个定时器到点。
func TestEnqueueDelayedSignalsDispatcher(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	e.enqueueDelayed(time.Now().Add(time.Minute), &Task{ID: 7, Stage: "stub"}, delayRecord(7))
	select {
	case <-e.delayCh:
	default:
		t.Fatal("入队应当给 delayCh 发一次信号")
	}

	// 信号满了也不能阻塞调用方（第二次发送走 default 分支）
	e.enqueueDelayed(time.Now().Add(time.Minute), &Task{ID: 7, Stage: "stub"}, delayRecord(7))
	e.enqueueDelayed(time.Now().Add(time.Minute), &Task{ID: 7, Stage: "stub"}, delayRecord(7))
}
