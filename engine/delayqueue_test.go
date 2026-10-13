package engine

import (
	"container/heap"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
)

func TestDelayHeapOrdering(t *testing.T) {
	now := time.Now()
	h := &delayHeap{}
	heap.Init(h)
	heap.Push(h, &delayedTask{at: now.Add(10 * time.Second)})
	heap.Push(h, &delayedTask{at: now.Add(2 * time.Second)})
	heap.Push(h, &delayedTask{at: now.Add(5 * time.Second)})

	want := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	for i, w := range want {
		got := heap.Pop(h).(*delayedTask).at
		if !got.Equal(now.Add(w)) {
			t.Fatalf("pop %d = %v, want %v", i, got.Sub(now), w)
		}
	}
	if h.Len() != 0 {
		t.Fatalf("heap should be empty, len=%d", h.Len())
	}
}

// 到点前运营动过这一行（跑完了 / 被标失败 / 已被别的路径投出去并被认领）→ **不能投**。
// 照旧快照投的话，`submitUpdates` 会把它无条件写回 pending，`claimTask`（要的正是 pending）
// 必定认领成功：跑完的任务被再跑一遍，"标失败"这个拦截手段也被绕过去。
func TestDelayDispatcherDropsTaskNoLongerPending(t *testing.T) {
	cases := []struct {
		name   string
		status models.TaskStatus
	}{
		{"已跑完", models.TaskStatusSuccess},
		{"已被标失败", models.TaskStatusFailed},
		{"已有一份在跑", models.TaskStatusProcessing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.mu.Lock()
			f.row["status"] = int64(tc.status)
			f.mu.Unlock()
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := delayEngine(t, f, pool)

			e.enqueueDelayed(time.Now().Add(-time.Second),
				&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/delayed"}, delayRecord(7))
			time.Sleep(80 * time.Millisecond)

			if main, urgent := pool.QueueDepths(); main+urgent != 0 {
				t.Fatalf("不在 pending 的行不该被投出去，实得队列深度 %d", main+urgent)
			}
			// 也不该写库（写就是把状态翻回 pending —— 那正是要避免的动作）
			if got := f.written(); got != "" {
				t.Fatalf("丢掉的任务不该写库：\n%s", got)
			}
			// 堆里也不留（丢掉就是丢掉，不是延后）
			e.delayMu.Lock()
			n := e.delayHeap.Len()
			e.delayMu.Unlock()
			if n != 0 {
				t.Fatalf("丢掉的任务不该留在延迟堆里，实得 %d", n)
			}
		})
	}
}

// 行已经被删掉（运营在后台删了任务）→ 什么都不做，别把它插回来。
func TestDelayDispatcherDropsDeletedRow(t *testing.T) {
	f := newFakeTaskDB()
	f.mu.Lock()
	f.noRows = true
	f.mu.Unlock()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := delayEngine(t, f, pool)

	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/delayed"}, delayRecord(7))
	time.Sleep(80 * time.Millisecond)

	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("行已删就不该投递，实得 %d", main+urgent)
	}
	if got := f.written(); got != "" {
		t.Fatalf("行已删就不该写库：\n%s", got)
	}
}

// 重读那一下撞上瞬态库错误：**不能丢**（行还是 pending，丢了就再没人投它），
// 也不能立刻重排（会退化成忙等）—— 放回堆里、隔一个回灌间隔再看。
func TestDelayDispatcherRetriesWhenRereadFails(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := delayEngine(t, f, pool)

	f.mu.Lock()
	f.failQueries = 1 // 让下面那次重读报错
	f.mu.Unlock()

	before := time.Now()
	e.enqueueDelayed(time.Now().Add(-time.Second),
		&Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/delayed"}, delayRecord(7))
	time.Sleep(80 * time.Millisecond)

	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatalf("重读失败时不该投递，实得 %d", main+urgent)
	}
	if got := f.written(); got != "" {
		t.Fatalf("重读失败时不该写库：\n%s", got)
	}
	e.delayMu.Lock()
	defer e.delayMu.Unlock()
	if e.delayHeap.Len() != 1 {
		t.Fatalf("重读失败的任务应当放回堆里等下一轮，实得 %d", e.delayHeap.Len())
	}
	if got := e.delayHeap[0].at; !got.After(before) {
		t.Fatalf("放回堆里的任务应当改到将来（避免忙等），实得 %v", got)
	}
}
