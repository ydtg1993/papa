package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// dyntickerEngine 造一个够跑动态 ticker 的引擎（只需要 ctx 与配置变更通道）。
func dyntickerEngine(t *testing.T) (*Engine, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{ctx: ctx, cancel: cancel, configChanged: make(chan struct{}, 1)}
	t.Cleanup(cancel)
	return e, cancel
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

// 间隔为 0 = 停用：一条都不该跑，协程只在 ctx.Done / 配置变更上等着。
func TestRunDynamicTickerDisabledInterval(t *testing.T) {
	e, _ := dyntickerEngine(t)
	var ticks atomic.Int64
	e.runDynamicTicker(func() time.Duration { return 0 }, nil, func() { ticks.Add(1) })

	time.Sleep(80 * time.Millisecond)
	if got := ticks.Load(); got != 0 {
		t.Fatalf("停用状态不该 tick，实得 %d", got)
	}

	// 无关的配置变更（改浏览器头之类）来了也不能把它唤醒成"在跑"
	e.configChanged <- struct{}{}
	time.Sleep(60 * time.Millisecond)
	if got := ticks.Load(); got != 0 {
		t.Fatalf("间隔仍为 0 时不该 tick，实得 %d", got)
	}
}

// 间隔从 0 变成正数（运行期把队列打开）：配置变更把它唤醒，立刻开始跑。
func TestRunDynamicTickerStartsOnConfigChange(t *testing.T) {
	e, _ := dyntickerEngine(t)
	var interval atomic.Int64 // 纳秒
	var ticks atomic.Int64

	e.runDynamicTicker(
		func() time.Duration { return time.Duration(interval.Load()) },
		nil /* 这两条队列没有"数据驱动"的间隔 */, func() { ticks.Add(1) },
	)
	time.Sleep(50 * time.Millisecond)
	if got := ticks.Load(); got != 0 {
		t.Fatalf("初始停用，不该 tick，实得 %d", got)
	}

	interval.Store(int64(10 * time.Millisecond))
	e.configChanged <- struct{}{}
	waitTicks(t, &ticks, 3)
}

// 间隔再变回 0（运行期把队列关掉）：ticker 停掉，计数冻住。
func TestRunDynamicTickerStopsWhenIntervalGoesAway(t *testing.T) {
	e, _ := dyntickerEngine(t)
	var interval atomic.Int64
	var ticks atomic.Int64

	interval.Store(int64(10 * time.Millisecond))
	e.runDynamicTicker(
		func() time.Duration { return time.Duration(interval.Load()) },
		nil /* 这两条队列没有"数据驱动"的间隔 */, func() { ticks.Add(1) },
	)
	waitTicks(t, &ticks, 2)

	interval.Store(0)
	e.configChanged <- struct{}{}
	// 等它在途的那一次 tick 落定
	time.Sleep(50 * time.Millisecond)

	base := ticks.Load()
	time.Sleep(120 * time.Millisecond)
	if got := ticks.Load(); got != base {
		t.Fatalf("停用后不该再 tick：从 %d 涨到了 %d", base, got)
	}
}

// 间隔变了（不是开关，是数值）：重建 ticker，按新节奏跑。
func TestRunDynamicTickerFollowsIntervalChange(t *testing.T) {
	e, _ := dyntickerEngine(t)
	var interval atomic.Int64
	var ticks atomic.Int64

	interval.Store(int64(time.Hour)) // 基本不会到点
	e.runDynamicTicker(
		func() time.Duration { return time.Duration(interval.Load()) },
		nil /* 这两条队列没有"数据驱动"的间隔 */, func() { ticks.Add(1) },
	)
	time.Sleep(50 * time.Millisecond)
	if got := ticks.Load(); got != 0 {
		t.Fatalf("1 小时的间隔不该在这点时间内 tick，实得 %d", got)
	}

	interval.Store(int64(10 * time.Millisecond))
	e.configChanged <- struct{}{}
	waitTicks(t, &ticks, 3)
}

// 引擎停机：协程退出，此后不再 onTick。
func TestRunDynamicTickerExitsOnCtxCancel(t *testing.T) {
	e, cancel := dyntickerEngine(t)
	var ticks atomic.Int64
	e.runDynamicTicker(func() time.Duration { return 10 * time.Millisecond }, nil, func() { ticks.Add(1) })
	waitTicks(t, &ticks, 2)

	cancel()
	time.Sleep(60 * time.Millisecond)
	base := ticks.Load()
	time.Sleep(120 * time.Millisecond)
	if got := ticks.Load(); got != base {
		t.Fatalf("ctx 取消后不该再 tick：从 %d 涨到了 %d", base, got)
	}
}

// 停用状态下停机也要退得干净（这时它等的是 ctx.Done 与 configChanged 两支）。
func TestRunDynamicTickerExitsWhileDisabled(t *testing.T) {
	e, cancel := dyntickerEngine(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.runDynamicTicker(func() time.Duration { return 0 }, nil, func() {})
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("停用状态下停机应当立刻退出")
	}
}
