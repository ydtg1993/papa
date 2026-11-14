package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// dyntickerEngine 造一个够跑动态 ticker 的引擎：只需要 ctx 与"叫醒"通道
// （原先是"配置变更"信号；热更取消之后，重算间隔的触发源就是 wake）。
func dyntickerEngine(t *testing.T) (*Engine, context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{ctx: ctx, cancel: cancel}
	wake := make(chan struct{}, 1)
	t.Cleanup(cancel)
	return e, cancel, wake
}

func waitTicks(t *testing.T, n *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for n.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("等不到 %d 次 tick，实得 %d", want, n.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// 间隔变了（不是开关，是数值）：重建 ticker，按新节奏跑。
func TestRunDynamicTickerFollowsIntervalChange(t *testing.T) {
	e, _, wake := dyntickerEngine(t)
	var interval atomic.Int64
	var ticks atomic.Int64

	interval.Store(int64(time.Hour)) // 基本不会到点
	e.runDynamicTicker(
		func() time.Duration { return time.Duration(interval.Load()) },
		wake, func() { ticks.Add(1) },
	)
	time.Sleep(50 * time.Millisecond)
	if got := ticks.Load(); got != 0 {
		t.Fatalf("1 小时的间隔不该在这点时间内 tick，实得 %d", got)
	}

	interval.Store(int64(10 * time.Millisecond))
	wake <- struct{}{}
	waitTicks(t, &ticks, 3)
}

// 引擎停机：协程退出，此后不再 onTick。
func TestRunDynamicTickerExitsOnCtxCancel(t *testing.T) {
	e, cancel, wake := dyntickerEngine(t)
	var ticks atomic.Int64
	e.runDynamicTicker(func() time.Duration { return 10 * time.Millisecond }, wake, func() { ticks.Add(1) })
	waitTicks(t, &ticks, 2)

	cancel()
	time.Sleep(60 * time.Millisecond)
	base := ticks.Load()
	time.Sleep(120 * time.Millisecond)
	if got := ticks.Load(); got != base {
		t.Fatalf("ctx 取消后不该再 tick：从 %d 涨到了 %d", base, got)
	}
}

// 长间隔等待中取消，必须等实际 ticker 协程退出。
func TestRunDynamicTickerExitsDuringLongWait(t *testing.T) {
	e, cancel, wake := dyntickerEngine(t)
	e.runDynamicTicker(func() time.Duration { return time.Hour }, wake, func() { t.Error("unexpected tick") })
	cancel()
	done := make(chan struct{})
	go func() { e.tickerWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ticker did not stop")
	}
}

// wake 突发要合并：启动恢复/批量提交会连续叫醒，每次重算节拍都是一条 MIN 聚合查询；
// 合并窗口内只该重算一次（加上最开始算间隔那一次）。
func TestRunDynamicTickerCoalescesWakeBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &Engine{ctx: ctx, cancel: cancel, wakeCoalesce: 40 * time.Millisecond}
	wake := make(chan struct{}, 64)

	var recalcs, ticks atomic.Int64
	e.runDynamicTicker(
		func() time.Duration { recalcs.Add(1); return time.Hour }, // 长间隔：只靠 wake 触发重算
		wake, func() { ticks.Add(1) },
	)

	// 等第一次 getInterval（启动那次）
	deadline := time.Now().Add(2 * time.Second)
	for recalcs.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("ticker 没启动")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 50; i++ {
		wake <- struct{}{}
	}
	time.Sleep(300 * time.Millisecond)

	if got := recalcs.Load(); got < 2 {
		t.Fatalf("窗口到期后应当重算一次节拍，实得 %d 次", got)
	} else if got > 3 {
		t.Fatalf("50 次 wake 被合并后最多重算 3 次（含启动那次），实得 %d 次", got)
	}
	if ticks.Load() != 0 {
		t.Fatalf("长间隔下不该 tick，实得 %d", ticks.Load())
	}
}
