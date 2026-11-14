package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
)

/* ---------- 延迟投递的 dispatcher ---------- */

// delayEngine 起一个延迟派发协程，返回可用的引擎。
func delayEngine(t *testing.T, f *fakeTaskDB, pool *workerpool.WorkerPool[*Task]) *Engine {
	t.Helper()
	e := submitEngine(t, f, pool)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	t.Cleanup(e.cancel)
	e.startPendingRecovery()
	t.Cleanup(func() { e.cancel(); e.tickerWG.Wait() })
	go e.delayDispatcher()
	return e
}

func delayRecord(id uint) models.CrawlerTask {
	return models.CrawlerTask{
		ID: id, Stage: "stub", Site: "stub", URL: "https://example.com/delayed",
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
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := delayEngine(t, f, pool)

	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/delayed"}, delayRecord(7))

	waitQueueDepth(t, pool, 1)
	// 已持久化 pending 只需派发，不重复写状态。
	if got := f.written(); got != "" {
		t.Fatalf("投递不应重复更新 pending：\n%s", got)
	}
}

// 未到点的任务不该被提前投递 —— 延迟投递的意义就是不占 worker 的并发位。
func TestDelayDispatcherHoldsFutureTask(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := delayEngine(t, f, pool)

	e.enqueueDelayed(time.Now().Add(time.Hour),
		&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/later"}, delayRecord(7))

	time.Sleep(60 * time.Millisecond)
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("未到点不该入队，实得 %d", main+urgent)
	}

	n := e.delays.Len()

	if n != 1 {
		t.Fatalf("任务应留在延迟堆里，实得 %d", n)
	}
}

// 等待期间来了更早的任务：要唤醒它重新计算等待时间，否则早的任务会被晚的拖住。
func TestDelayDispatcherRecomputesOnEarlierTask(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := delayEngine(t, f, pool)

	// 先排一个 1 小时后的
	e.enqueueDelayed(time.Now().Add(time.Hour),
		&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/later"}, delayRecord(7))
	time.Sleep(30 * time.Millisecond) // 让它进入定时器等待

	// 再来一个已经到点的
	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 8, Stage: "stub", Site: "stub", URL: "https://example.com/now"}, delayRecord(8))

	waitQueueDepth(t, pool, 1)

	n := e.delays.Len()

	if n != 1 {
		t.Fatalf("只有到点的那条该被取走，延迟堆应剩 1 条，实得 %d", n)
	}
}

// 引擎停机 → 派发协程退出，此后不再投递任何东西。
func TestDelayDispatcherExitsOnCtxCancel(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8)
	e := delayEngine(t, f, pool)

	e.cancel()
	time.Sleep(80 * time.Millisecond) // 让协程走完 ctx.Done 那一支

	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/dead"}, delayRecord(7))
	time.Sleep(80 * time.Millisecond)

	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("停机后不该再投递，实得 %d", main+urgent)
	}
}

// 延迟入队要主动叫醒派发协程（非阻塞信号），否则新任务得等上一个定时器到点。
func TestEnqueueDelayedSignalsDispatcher(t *testing.T) {
	f := newFakeTaskDB()
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8))

	e.enqueueDelayed(time.Now().Add(time.Minute), &Task{ID: 7, Stage: "stub", Site: "stub"}, delayRecord(7))
	select {
	case <-e.delays.Wake():
	default:
		t.Fatal("入队应当给 delayCh 发一次信号")
	}

	// 信号满了也不能阻塞调用方（第二次发送走 default 分支）
	e.enqueueDelayed(time.Now().Add(time.Minute), &Task{ID: 7, Stage: "stub", Site: "stub"}, delayRecord(7))
	e.enqueueDelayed(time.Now().Add(time.Minute), &Task{ID: 7, Stage: "stub", Site: "stub"}, delayRecord(7))
}
