package track

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/internal/workerpool"
)

type fakeTask struct{ key string }

func (f fakeTask) Unique() string { return f.key }

// updateStats 是统计的全部逻辑：per-worker 与全局的累计值、最大/最小耗时、平均耗时。
// 这里直接喂 Activity（不经过通道），断言是确定性的。
func TestUpdateStatsAccumulatesPerWorkerAndGlobal(t *testing.T) {
	m := NewStatsQueue[fakeTask](nil)

	m.updateStats(workerpool.Activity{WorkerID: 0, Duration: 10 * time.Millisecond})
	m.updateStats(workerpool.Activity{WorkerID: 0, Duration: 30 * time.Millisecond, Error: errors.New("boom")})
	m.updateStats(workerpool.Activity{WorkerID: 1, Duration: 50 * time.Millisecond})

	// per-worker：worker 0 两条（一成功一失败），min/max 各自独立
	w0, ok := m.GetWorkerStats(0)
	if !ok {
		t.Fatal("worker 0 应当有统计")
	}
	if w0.TotalTasks != 2 || w0.FailedTasks != 1 {
		t.Fatalf("worker0 tasks/failed = %d/%d, want 2/1", w0.TotalTasks, w0.FailedTasks)
	}
	if w0.TotalTime != 40*time.Millisecond {
		t.Fatalf("worker0 totalTime = %v, want 40ms", w0.TotalTime)
	}
	if w0.MaxTime != 30*time.Millisecond || w0.MinTime != 10*time.Millisecond {
		t.Fatalf("worker0 max/min = %v/%v, want 30ms/10ms", w0.MaxTime, w0.MinTime)
	}

	// worker 1 是新 worker：MinTime 初始化为它自己那一条的耗时
	w1, ok := m.GetWorkerStats(1)
	if !ok || w1.TotalTasks != 1 || w1.MinTime != 50*time.Millisecond {
		t.Fatalf("worker1 = %+v, ok=%v", w1, ok)
	}

	g := m.GetGlobalStats()
	if g.TotalTasks != 3 || g.TotalFailed != 1 {
		t.Fatalf("global tasks/failed = %d/%d, want 3/1", g.TotalTasks, g.TotalFailed)
	}
	if g.TotalTime != 90*time.Millisecond {
		t.Fatalf("global totalTime = %v, want 90ms", g.TotalTime)
	}
	if g.MaxTime != 50*time.Millisecond || g.MinTime != 10*time.Millisecond {
		t.Fatalf("global max/min = %v/%v, want 50ms/10ms", g.MaxTime, g.MinTime)
	}
	if g.AvgTime != 30*time.Millisecond {
		t.Fatalf("global avg = %v, want 30ms", g.AvgTime)
	}
}

// GetWorkerStats 对不存在的 worker 返回 false：监控页据此区分"这个 worker 还没跑过"与"跑了 0 个"。
func TestGetWorkerStatsUnknownWorker(t *testing.T) {
	m := NewStatsQueue[fakeTask](nil)
	if _, ok := m.GetWorkerStats(42); ok {
		t.Fatal("没跑过的 worker 不该存在统计")
	}
	m.updateStats(workerpool.Activity{WorkerID: 3, Duration: time.Millisecond})

	all := m.GetAllWorkerStats()
	if len(all) != 1 {
		t.Fatalf("GetAllWorkerStats = %v, want 只有 worker 3", all)
	}
	// 返回的是副本：改它不该动到内部状态
	cp := all[3]
	cp.TotalTasks = 999
	if again, _ := m.GetWorkerStats(3); again.TotalTasks != 1 {
		t.Fatalf("GetAllWorkerStats 应返回副本，实得 %+v", again)
	}
}

// 端到端：Start 起消费协程，池子的活动经通道流进统计。
func TestStartConsumesPoolActivities(t *testing.T) {
	pool := workerpool.NewWorkerPool[fakeTask](1, 8, 1)
	stats := NewStatsQueue[fakeTask](pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stats.Start(ctx)

	pool.Start(context.Background(), func(_ context.Context, _ fakeTask) error { return nil })
	defer pool.Stop(time.Second)

	if err := pool.Submit(fakeTask{key: "a"}); err != nil {
		t.Fatalf("Submit = %v", err)
	}

	// 活动是异步消费的，轮询等它落地
	deadline := time.Now().Add(3 * time.Second)
	for stats.GetGlobalStats().TotalTasks != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("统计没有跟上：%+v", stats.GetGlobalStats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := stats.GetAllWorkerStats(); len(got) != 1 {
		t.Fatalf("GetAllWorkerStats = %v, want 1 个 worker", got)
	}
}
