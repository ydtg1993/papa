package browser

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

/* ---------- Browser 的使用计数与存活判定 ---------- */

// 使用计数与最后使用时间：空闲回收（max_idle_time）就是拿 lastUsed 当基准的，
// 计数则是"这个浏览器被复用了多少次"的观测值。
func TestBrowserUsageCounters(t *testing.T) {
	b := &Browser{}

	if got := b.GetUseCount(); got != 0 {
		t.Fatalf("初始使用次数 = %d, want 0", got)
	}
	if !b.GetLastUsed().IsZero() {
		t.Fatal("初始的最后使用时间应是零值")
	}

	b.markUsed()
	b.markUsed()
	if got := b.GetUseCount(); got != 2 {
		t.Fatalf("使用次数 = %d, want 2", got)
	}
	first := b.GetLastUsed()
	if first.IsZero() {
		t.Fatal("markUsed 应记下时间")
	}

	time.Sleep(2 * time.Millisecond)
	b.markIdle()
	if b.GetLastUsed().Before(first) {
		t.Fatal("markIdle 应把空闲起点往后推")
	}
	// markIdle 不动使用计数
	if got := b.GetUseCount(); got != 2 {
		t.Fatalf("markIdle 不该改使用次数，实得 %d", got)
	}
}

// IsAlive：优先用测试钩子；没有钩子且 Browser 为 nil 时是死的（不能对 nil 发 CDP 探测）。
func TestBrowserIsAlive(t *testing.T) {
	if (&Browser{aliveOverride: boolPtr(true)}).IsAlive() != true {
		t.Fatal("钩子为 true 时应报存活")
	}
	if (&Browser{aliveOverride: boolPtr(false)}).IsAlive() != false {
		t.Fatal("钩子为 false 时应报死亡")
	}
	if (&Browser{}).IsAlive() {
		t.Fatal("没接 rod 实例时应报死亡，而不是去解引用 nil")
	}
}

// 零值 Browser 的 Close 要安全：池子回收死实例时会走到这里。
func TestBrowserCloseOnZeroValue(t *testing.T) {
	(&Browser{}).Close() // 不该 panic
}

/* ---------- 池子的只读访问器与热更 ---------- */

// SetHeaders 是 copy-on-write 且**拷入**：调用方之后改动自己的 map 不能影响池子
// （引擎每次热更都会重新算一份 headers，不拷贝就成共享可写了）。
func TestPoolHeadersAreCopied(t *testing.T) {
	p := newTestPool(1, 0, nil)

	src := map[string]string{"X-A": "1"}
	p.SetHeaders(src)
	src["X-A"] = "changed"
	src["X-B"] = "new"

	got := p.headersSnapshot()
	if got["X-A"] != "1" {
		t.Fatalf("池子里的 headers 被外部改动影响了：%+v", got)
	}
	if _, ok := got["X-B"]; ok {
		t.Fatalf("外部新增的键不该进池子：%+v", got)
	}

	// 快照本身**不是**副本（见 headersSnapshot 的注释：共享只读，调用方不得修改）。
	// 真正挡住"外部改动渗进池子"的是 SetHeaders 那一次拷入 —— 上面两条断言的就是它。
	// 这里只钉住"改快照会改到池子"这个已知契约，免得有人误以为拿到的是副本。
	got["X-A"] = "mutated"
	if again := p.headersSnapshot(); again["X-A"] != "mutated" {
		t.Fatalf("headersSnapshot 按契约返回共享 map（调用方只读）：%+v", again)
	}
	p.SetHeaders(map[string]string{"X-A": "1"})

	// 传 nil 时得到空 map 而不是 nil（新建 page 时直接 range 它）
	p.SetHeaders(nil)
	if p.headersSnapshot() == nil {
		t.Fatal("SetHeaders(nil) 之后快照不该是 nil")
	}
}

// maxIdleTime 热更：SetMaxIdleTime 之后池子读到的是新值。
func TestPoolMaxIdleTimeHotReload(t *testing.T) {
	p := newTestPool(1, 0, nil)
	if got := p.maxIdleDuration(); got != 0 {
		t.Fatalf("初始应为 0（不回收），实得 %v", got)
	}

	p.SetMaxIdleTime(3 * time.Second)
	if got := p.maxIdleDuration(); got != 3*time.Second {
		t.Fatalf("热更后 = %v, want 3s", got)
	}
	p.Close()
}

/* ---------- 归还 ---------- */

// 不属于任何池子的浏览器、以及 nil，都要被拒（并顺手关掉，避免泄漏）。
func TestPutRejectsNilAndForeignBrowser(t *testing.T) {
	p := newTestPool(1, 0, nil)

	if err := p.Put(nil); err == nil {
		t.Fatal("Put(nil) 应当报错")
	}
	foreign := &Browser{aliveOverride: boolPtr(true)}
	if err := p.Put(foreign); err == nil {
		t.Fatal("不属于任何池子的浏览器应当被拒")
	}
	if len(p.browsers) != 0 {
		t.Fatal("被拒的浏览器不该进空闲通道")
	}
}

// 池子已关闭时归还：直接关掉并报错，不再进通道（通道已经 close 了）。
func TestPutAfterCloseClosesBrowser(t *testing.T) {
	p := newTestPool(1, 0, nil)
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Close()

	if err := p.Put(b); err == nil {
		t.Fatal("池子已关闭时归还应当报错")
	}
}

// 死实例归还时直接回收、不进空闲通道 —— 否则下一个 Get 拿到一个死浏览器。
func TestPutDeadBrowserDoesNotEnqueue(t *testing.T) {
	p := newTestPool(1, 0, nil)
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	*b.aliveOverride = false

	if err := p.Put(b); err != nil {
		t.Fatalf("归还死实例不该报错（静默回收）：%v", err)
	}
	if len(p.browsers) != 0 {
		t.Fatal("死实例不该留在空闲通道里")
	}
	if got := p.proxyAlive.Load(); got != 0 {
		t.Fatalf("存活计数应被扣掉，实得 %d", got)
	}
}

// usable 的三条判断：活着且没超时 → 复用；死了 / 空闲超时 → 关掉并扣计数。
func TestPoolUsableVerdicts(t *testing.T) {
	p := newTestPool(1, 0, nil)

	var alive atomic.Int64
	fresh := testBrowser(true, true)
	fresh.markIdle()
	if got := p.usable(fresh, &alive, time.Minute); got == nil {
		t.Fatal("活的且没超时应被复用")
	}

	dead := testBrowser(true, false)
	alive.Store(1)
	if got := p.usable(dead, &alive, time.Minute); got != nil {
		t.Fatal("死实例不该被复用")
	}
	if alive.Load() != 0 {
		t.Fatalf("回收死实例应扣掉存活计数，实得 %d", alive.Load())
	}

	// 空闲超过阈值
	expired := testBrowser(true, true)
	expired.mu.Lock()
	expired.lastUsed = time.Now().Add(-time.Hour)
	expired.mu.Unlock()
	alive.Store(1)
	if got := p.usable(expired, &alive, time.Minute); got != nil {
		t.Fatal("空闲超时的实例不该被复用")
	}
	if alive.Load() != 0 {
		t.Fatalf("回收超时实例应扣掉存活计数，实得 %d", alive.Load())
	}

	// 阈值为 0 表示不按空闲回收
	old := testBrowser(true, true)
	old.mu.Lock()
	old.lastUsed = time.Now().Add(-time.Hour)
	old.mu.Unlock()
	alive.Store(1)
	if got := p.usable(old, &alive, 0); got == nil {
		t.Fatal("阈值为 0 时不该按空闲回收")
	}
}

/* ---------- 回收协程的节奏 ---------- */

// 扫描间隔取 max_idle_time 的一半，并夹在 [1s, 30s]：
// 太密是白烧 CPU，太疏则空闲实例回收不及时（"用完就关"的意图就落空了）。
func TestReaperIntervalClamped(t *testing.T) {
	cases := []struct {
		maxIdle time.Duration
		want    time.Duration
	}{
		{0, 0}, // 不回收 → 不起协程
		{-time.Second, 0},
		{time.Second, time.Second}, // 半秒 → 抬到 1s
		{10 * time.Second, 5 * time.Second},
		{time.Minute, 30 * time.Second}, // 30s → 压到 30s
		{time.Hour, 30 * time.Second},   // 半小时 → 压到 30s
	}
	for _, c := range cases {
		p := newTestPool(1, 0, nil)
		p.maxIdle.Store(int64(c.maxIdle))
		if got := p.reaperInterval(); got != c.want {
			t.Errorf("maxIdle=%v: reaperInterval = %v, want %v", c.maxIdle, got, c.want)
		}
	}
}

// max_idle_time > 0 时回收协程起得来；重复 Set 不该起第二个。
func TestEnsureReaperStartsOnce(t *testing.T) {
	p := newTestPool(1, 0, nil)

	p.SetMaxIdleTime(2 * time.Second)
	p.reaperMu.Lock()
	first := p.reaperStop
	p.reaperMu.Unlock()
	if first == nil {
		t.Fatal("max_idle_time > 0 时应起回收协程")
	}

	p.SetMaxIdleTime(3 * time.Second)
	p.reaperMu.Lock()
	second := p.reaperStop
	p.reaperMu.Unlock()
	if second != first {
		t.Fatal("已有回收协程时不该再起一个")
	}

	p.Close()
	select {
	case <-first:
	default:
		t.Fatal("Close 应停掉回收协程")
	}
}

/* ---------- 拷贝工具 ---------- */

func TestCopyMap(t *testing.T) {
	if got := copyMap(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil 应得到空 map：%+v", got)
	}
	src := map[string]string{"a": "1", "b": "2"}
	got := copyMap(src)
	if len(got) != 2 || got["a"] != "1" {
		t.Fatalf("copyMap = %+v", got)
	}
	got["a"] = "changed"
	if src["a"] != "1" {
		t.Fatal("copyMap 应是深拷贝")
	}
}

func TestCopyCookies(t *testing.T) {
	if got := copyCookies(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil 应得到空切片：%+v", got)
	}

	src := []*proto.NetworkCookieParam{{Name: "sid", Value: "v", Domain: "example.com", Expires: proto.TimeSinceEpoch(0)}}
	got := copyCookies(src)
	if len(got) != 1 || got[0].Name != "sid" {
		t.Fatalf("copyCookies = %+v", got)
	}
	// 元素也要是新的指针：改副本不能动到源
	got[0].Value = "changed"
	if src[0].Value != "v" {
		t.Fatal("copyCookies 应逐个复制元素（不是只复制切片头）")
	}
}

/* ---------- 关闭后的行为 ---------- */

// 关闭后 Get / GetDirect 都立刻报错，不阻塞。
func TestGetAfterClose(t *testing.T) {
	p := newTestPool(1, 1, nil)
	p.Close()

	if _, err := p.Get(context.Background()); err == nil {
		t.Fatal("关闭后 Get 应当报错")
	}
	if _, err := p.GetDirect(context.Background()); err == nil {
		t.Fatal("关闭后 GetDirect 应当报错")
	}
}

// 已取消的 ctx 不该让 Get 挂住（它要能立刻返回 ctx 的错误）。
func TestGetWithCancelledContext(t *testing.T) {
	p := newTestPool(1, 0, nil) // 容量 1
	// 先占满，让 Get 只能走等待那条路
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := p.Get(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ctx 已取消时 Get 应当报错")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get 应当被 ctx 取消唤醒，而不是一直挂着")
	}
	p.Close()
}
