package engine

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// 空闲回收：lastSubmit 超了 idle 且空着 → 回收；队列非空 → 不回收。
func TestPoolRegistryReapIdle(t *testing.T) {
	r := newPoolRegistry()
	idlePool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	busyPool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	r.set("idle", idlePool)
	r.set("busy", busyPool)
	// busy 池里压着一条任务（队列非空）
	if err := busyPool.Submit(&Task{Stage: "busy", URL: "https://example.com"}); err != nil {
		t.Fatalf("Submit = %v", err)
	}
	// 两个池的 lastSubmit 都回拨到 2 分钟前
	backdate := time.Now().Add(-2 * time.Minute)
	r.lastSubmit["idle"] = backdate
	r.lastSubmit["busy"] = backdate

	reaped := r.reapIdle(time.Minute, time.Second)
	if len(reaped) != 1 || reaped[0] != "idle" {
		t.Fatalf("只该回收 idle，实得 %v", reaped)
	}
	if r.get("idle") != nil {
		t.Fatal("idle 池应被回收")
	}
	if r.get("busy") == nil {
		t.Fatal("busy 池不该被回收（队列非空）")
	}
}

// 每次 getOrCreate 都要刷新 lastSubmit（空闲回收靠它判断闲置时长）。
func TestPoolRegistryGetOrCreateRefreshesLastSubmit(t *testing.T) {
	r := newPoolRegistry()
	mk := func() *workerpool.WorkerPool[*Task] {
		return workerpool.NewWorkerPool[*Task](1, 8, 1)
	}
	r.getOrCreate("s", mk)
	if r.lastSubmit["s"].IsZero() {
		t.Fatal("首次 getOrCreate 应记 lastSubmit")
	}

	// 回拨后再 getOrCreate 一次，应刷新到接近现在
	r.lastSubmit["s"] = time.Now().Add(-time.Hour)
	r.getOrCreate("s", mk)
	if since := time.Since(r.lastSubmit["s"]); since > time.Second {
		t.Fatalf("getOrCreate 应把 lastSubmit 刷新到接近现在，实得 %v 前", since)
	}
}
