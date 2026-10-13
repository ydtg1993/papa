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
	if len(reaped) != 1 || reaped["idle"] == nil {
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

// 停机位：shutdown 在同一把锁里"置位 + 取快照"，此后的 getOrCreate 一律拒绝建池。
//
// 这是 F-01 的结构性收口 —— 置位与取快照分开做的话，两步之间进来的投递会建出一个
// 既不在快照里（Stop 不会停它）、又有活 worker 的池；框架自己的收尾协程（deliverDelayed /
// drainSpilled）恰好就在那个窗口里投递。
func TestPoolRegistryShutdownRefusesNewPools(t *testing.T) {
	r := newPoolRegistry()
	existing := workerpool.NewWorkerPool[*Task](1, 8, 1)
	r.set("stub:stub", existing)

	if r.isStopped() {
		t.Fatal("刚建的表不该是停机态")
	}

	snap := r.shutdown()
	if len(snap) != 1 || snap["stub:stub"] != existing {
		t.Fatalf("快照应含停机前登记的池，实得 %v", snap)
	}
	if !r.isStopped() {
		t.Fatal("shutdown 之后应为停机态")
	}

	// 停机之后：不再建池（create 一次都不该被调用），但表里已有的池照旧取得到 ——
	// 它的 workers 还在跑，需要被 Stop 端（Engine.Stop 拿到的是 shutdown 的那份快照）。
	created := 0
	if p := r.getOrCreate("other:stage", func() *workerpool.WorkerPool[*Task] {
		created++
		return workerpool.NewWorkerPool[*Task](1, 8, 1)
	}); p != nil {
		t.Fatalf("停机之后不该再建池，实得 %v", p)
	}
	if created != 0 {
		t.Fatalf("create 不该被调用，实得 %d 次", created)
	}
	if r.get("stub:stub") != existing {
		t.Fatal("已登记的池该照旧取得到")
	}
	// 重复 shutdown 幂等（Stop 可能被调多次）
	if again := r.shutdown(); len(again) != 1 {
		t.Fatalf("重复 shutdown 应返回同一批池，实得 %v", again)
	}
}
