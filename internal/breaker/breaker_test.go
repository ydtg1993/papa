package breaker

import (
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/core"
)

// newTest 造一个时钟可控的熔断器 —— 窗口滑动全靠时间，用真 sleep 测会又慢又脆。
func newTest(cfg Config) (b *Breaker, advance func(time.Duration)) {
	b = New(cfg, nil)
	var mu sync.Mutex
	now := time.Unix(1700000000, 0)
	b.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return b, func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

func TestTripsAtThreshold(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 3})

	for i := 0; i < 2; i++ {
		if b.RecordFailure("catalog") {
			t.Fatalf("第 %d 次失败就触发了，阈值是 3", i+1)
		}
	}
	if b.Paused() {
		t.Fatal("没到阈值不该暂停")
	}
	if !b.RecordFailure("catalog") {
		t.Fatal("第 3 次失败应当触发熔断")
	}
	if !b.Paused() {
		t.Fatal("触发后应处于暂停态")
	}

	st := b.Status()
	if st.Stage != "catalog" || st.Failures != 3 || !st.Paused || st.InWindow != 3 {
		t.Fatalf("status = %+v, want stage=catalog failures=3 in_window=3 paused", st)
	}
	if st.Threshold != 3 || st.Window != time.Minute {
		t.Fatalf("status 应带上配置：%+v", st)
	}
}

// 窗口滑走之后老失败不再计入 —— 否则「几小时前失败过 50 次」会让熔断永远触发。
func TestFailuresFallOutOfWindow(t *testing.T) {
	b, advance := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 3})

	b.RecordFailure("s")
	b.RecordFailure("s")
	advance(2 * time.Minute)

	if got := b.Sum(); got != 0 {
		t.Fatalf("窗口滑走后 Sum = %d, want 0", got)
	}
	if b.RecordFailure("s") {
		t.Fatal("老失败不该把新失败顶到阈值")
	}
	if b.Paused() {
		t.Fatal("不该暂停")
	}
}

// 触发那一刻的现场（Stage/Failures/Reason）是快照，窗口滑走也不改写 ——
// 排查时要看的是"当时为什么断的"。
func TestTripSnapshotSurvivesWindowSlide(t *testing.T) {
	b, advance := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 2})
	b.RecordFailure("detail")
	b.RecordFailure("detail")
	advance(10 * time.Minute)

	st := b.Status()
	if st.Failures != 2 || st.Stage != "detail" || st.Reason == "" {
		t.Fatalf("现场快照被改写了：%+v", st)
	}
	if st.InWindow != 0 {
		t.Fatalf("InWindow 是实时值，应当已滑到 0，实得 %d", st.InWindow)
	}
}

// 暂停时 Wait 阻塞、Resume 放行 —— 闸门的核心语义。
func TestWaitBlocksUntilResume(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 1})
	b.RecordFailure("s")

	done := make(chan bool, 1)
	go func() { done <- b.Wait(make(chan struct{})) }()

	select {
	case <-done:
		t.Fatal("暂停中 Wait 不该返回")
	case <-time.After(50 * time.Millisecond):
	}

	if !b.Resume() {
		t.Fatal("Resume 应返回 true")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("Resume 后 Wait 应返回 true（继续干活）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resume 没有唤醒 Wait")
	}
}

// 暂停 → 恢复 → 再暂停 → 再恢复。
//
// 回归点：resumeCh 必须在 Pause 时**换一个新的**。只 close 不复用的话，第二次暂停时
// 那个 channel 早就关着了，select 立刻命中，"暂停"当场被放行 —— 熔断形同虚设。
func TestPauseResumeCycleDoesNotLeakOldSignal(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 1})

	for round := 1; round <= 3; round++ {
		if !b.RecordFailure("s") {
			t.Fatalf("第 %d 轮没触发", round)
		}
		if !b.Paused() {
			t.Fatalf("第 %d 轮应处于暂停态", round)
		}

		done := make(chan bool, 1)
		go func() { done <- b.Wait(make(chan struct{})) }()
		select {
		case <-done:
			t.Fatalf("第 %d 轮：暂停中 Wait 不该返回（是不是复用了上一轮已经 close 的 resumeCh？）", round)
		case <-time.After(30 * time.Millisecond):
		}

		if !b.Resume() {
			t.Fatalf("第 %d 轮 Resume 应返回 true", round)
		}
		select {
		case ok := <-done:
			if !ok {
				t.Fatalf("第 %d 轮 Resume 后应放行", round)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 轮 Resume 没唤醒 Wait", round)
		}
	}
}

// stop 到达时暂停中的 worker 必须能退出去 —— 否则关停会被暂停卡死。
func TestWaitReturnsFalseOnStop(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 1})
	b.RecordFailure("s")

	stop := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- b.Wait(stop) }()
	time.Sleep(30 * time.Millisecond)
	close(stop)

	select {
	case ok := <-done:
		if ok {
			t.Fatal("stop 关闭后 Wait 应返回 false（该停机了），而不是 true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop 没有唤醒 Wait")
	}
}

// 未启用时任何调用都不改变状态：不计数、不暂停。
func TestDisabledNeverTrips(t *testing.T) {
	b, _ := newTest(Config{Enabled: false, Window: time.Minute, Threshold: 1})

	for i := 0; i < 100; i++ {
		if b.RecordFailure("s") {
			t.Fatal("未启用不该触发")
		}
	}
	if b.Paused() {
		t.Fatal("未启用不该暂停")
	}
	if got := b.Sum(); got != 0 {
		t.Fatalf("未启用不该计数，实得 %d", got)
	}
	if st := b.Status(); st.Paused || st.InWindow != 0 {
		t.Fatalf("未启用时状态应干净：%+v", st)
	}
}

// 触发时回调一次，且**只一次** —— 之后每条失败都再喊一次就把人淹了。
func TestOnTripFiresOnce(t *testing.T) {
	var n int
	var got core.BreakerStatus
	var mu sync.Mutex
	b := New(Config{Enabled: true, Window: time.Minute, Threshold: 1}, func(st core.BreakerStatus) {
		mu.Lock()
		n++
		got = st
		mu.Unlock()
	})

	for i := 0; i < 5; i++ {
		b.RecordFailure("catalog")
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("onTrip 调了 %d 次, want 1", n)
	}
	if got.Stage != "catalog" || !got.Paused {
		t.Fatalf("告警里带的状态 = %+v", got)
	}
}

// 手动 Pause 走同一套闸门；本来就暂停时不再重复置位。
func TestManualPause(t *testing.T) {
	b, _ := newTest(Config{Enabled: true, Window: time.Minute, Threshold: 99})

	if !b.Pause("人工排查") {
		t.Fatal("首次 Pause 应返回 true")
	}
	if b.Pause("再来一次") {
		t.Fatal("已在暂停态，Pause 应返回 false")
	}
	if st := b.Status(); !st.Paused || st.Reason != "人工排查" {
		t.Fatalf("status = %+v", st)
	}
	if !b.Resume() || b.Resume() {
		t.Fatal("Resume 首次 true、再次 false")
	}
}

// nil 接收者（引擎没配熔断、或测试里直接构造 Engine）任何方法都不能 panic。
// 这条是有意为之：调用处（notifyFailure / waitGate）就不必到处判空。
func TestNilReceiverIsSafe(t *testing.T) {
	var b *Breaker

	if b.Enabled() || b.Paused() || b.RecordFailure("s") || b.Resume() || b.Pause("x") {
		t.Fatal("nil 熔断器不该有任何一个方法返回 true")
	}
	if !b.Wait(nil) {
		t.Fatal("nil 熔断器不该闸住（Wait 必须返回 true）")
	}
	if b.Sum() != 0 {
		t.Fatal("nil 熔断器 Sum 应为 0")
	}
	if st := b.Status(); st != (core.BreakerStatus{}) {
		t.Fatalf("nil 熔断器状态应为零值，实得 %+v", st)
	}
}
