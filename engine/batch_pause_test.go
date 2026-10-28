package engine

import (
	"context"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/models"
)

// 批间停顿必须在停机信号到来时立刻返回 false —— 启动恢复的协程在 tickerWG 里，
// 优雅关停不能被一个 30m 的停顿卡住（Engine.Stop 第一步是 cancel，然后等 tickerWG）。
func TestSleepBatchPauseInterruptible(t *testing.T) {
	e := &Engine{}
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx

	done := make(chan bool, 1)
	go func() { done <- e.sleepBatchPause(time.Hour) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("停机信号后应返回 false（停止继续分批）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("批间停顿没有被停机信号打断")
	}
}

// 短停顿照常等满；0/负数不停顿。
func TestSleepBatchPauseSemantics(t *testing.T) {
	e := &Engine{ctx: context.Background()}

	start := time.Now()
	if !e.sleepBatchPause(30 * time.Millisecond) {
		t.Fatal("正常等待应返回 true")
	}
	if elapsed := time.Since(start); elapsed < 25*time.Millisecond {
		t.Fatalf("停顿过短：%v，want >= 25ms", elapsed)
	}
	if !e.sleepBatchPause(0) || !e.sleepBatchPause(-time.Second) {
		t.Fatal("0/负数应不停顿并返回 true")
	}
}

// processInBatches 在「这一批满了、可能还有下一批」时才停顿；最后一批/空批不停。
// 用假库 3 行 + batch=1：三段有行（每段后停顿）+ 一段空 → 2 次停顿。
func TestProcessInBatchesHonorsBatchPause(t *testing.T) {
	f := newFakeTaskDB()
	f.rows = []fakeRow{f.rowWithID(1), f.rowWithID(2), f.rowWithID(3)}
	f.maxQueries = 20
	e, _ := urgentEngine(t, f)
	e.ctx = context.Background()

	var processed int
	start := time.Now()
	n, err := e.processInBatches(e.recoverQueueQuery(), 1, 1, 20*time.Millisecond, func(*models.CrawlerTask) bool {
		processed++
		return true
	})
	if err != nil {
		t.Fatalf("processInBatches = %v", err)
	}
	if n != 3 || processed != 3 {
		t.Fatalf("processed = %d/%d, want 3/3", n, processed)
	}
	// 3 行按 batch=1 分 3 批；前 2 批「满了」才停（第 3 批后下一段查询为空，但代码在
	// 返回空批之前就已经因为「本批满」停过一次 → 实际是 3 次停顿），阈值取 50ms 兜底。
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("批间停顿没有被执行：耗时 %v，want >= 50ms", elapsed)
	}
}
