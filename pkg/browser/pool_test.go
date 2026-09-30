package browser

import (
	"context"
	"sync"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

// aliveB 构造一个 IsAlive 恒为 v 的测试浏览器（不启动真实 Chrome）。
func testBrowser(useProxy, alive bool) *Browser {
	return &Browser{useProxy: useProxy, aliveOverride: boolPtr(alive)}
}

// countingFactory 记录工厂被调用时传入的 useProxy 序列（线程安全）。
type countingFactory struct {
	mu    sync.Mutex
	calls []bool
}

func (f *countingFactory) new(useProxy bool) (*Browser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, useProxy)
	f.mu.Unlock()
	return testBrowser(useProxy, true), nil
}

func (f *countingFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newTestPool 构建一个懒创建、工厂可注入的测试池。
func newTestPool(size, directSize int, factory func(bool) (*Browser, error)) *Pool {
	p := &Pool{cfg: PoolConfig{Size: size, DirectSize: directSize}}
	if factory != nil {
		p.newBrowserFn = factory
	} else {
		p.newBrowserFn = func(useProxy bool) (*Browser, error) { return testBrowser(useProxy, true), nil }
	}
	h := map[string]string{}
	p.headers.Store(&h)
	p.active.Store(p.newCore(size, directSize))
	return p
}

func TestPoolLazyCreation(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(2, 0, f.new)

	if f.count() != 0 {
		t.Fatalf("newCore created %d browsers, want 0 (lazy)", f.count())
	}

	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if f.count() != 1 {
		t.Fatalf("after first Get, created %d, want 1", f.count())
	}
	if !b.useProxy {
		t.Fatalf("Get returned useProxy=%v, want true", b.useProxy)
	}
}

func TestPoolGetBlocksWhenAtCapacity(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(2, 0, f.new)

	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count() != 2 {
		t.Fatalf("created %d, want 2", f.count())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Get(ctx); err == nil {
		t.Fatal("expected timeout on third Get at capacity")
	}
	if f.count() != 2 {
		t.Fatalf("third Get created a browser: count=%d", f.count())
	}
}

func TestPoolPutReusesAliveBrowser(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(1, 0, f.new)

	b1, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Put(b1); err != nil {
		t.Fatal(err)
	}
	b2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("reused browser but created %d, want 1", f.count())
	}
	if b2 != b1 {
		t.Fatal("expected same browser instance to be reused")
	}
}

func TestPoolPutDeadClosesAndNextGetRecreates(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(1, 0, f.new)

	b1, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 模拟浏览器崩溃后归还：不应入队，也不应立刻重建
	b1.aliveOverride = boolPtr(false)
	if err := p.Put(b1); err != nil {
		t.Fatal(err)
	}
	if f.count() != 1 {
		t.Fatalf("Put of dead browser created a new one: count=%d, want 1", f.count())
	}

	b2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 2 {
		t.Fatalf("next Get should recreate: count=%d, want 2", f.count())
	}
	if b2 == b1 {
		t.Fatal("expected a new browser, not the dead one")
	}
}

func TestPoolIdleReapOnGet(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(1, 0, f.new)
	p.maxIdle.Store(int64(time.Second))

	b1, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Put(b1); err != nil {
		t.Fatal(err)
	}
	// 把空闲起始时间拨回过去，模拟长期空闲
	b1.lastUsed = time.Now().Add(-2 * time.Second)

	b2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.count() != 2 {
		t.Fatalf("idle browser should be reaped and recreated: count=%d, want 2", f.count())
	}
	if b2 == b1 {
		t.Fatal("expected a fresh browser, not the stale idle one")
	}
}

func TestPoolReapChannel(t *testing.T) {
	p := newTestPool(2, 0, nil)
	core := p.active.Load()

	stale := testBrowser(true, true)
	stale.lastUsed = time.Now().Add(-time.Minute)
	fresh := testBrowser(true, true)
	fresh.lastUsed = time.Now()

	core.browsers <- stale
	core.browsers <- fresh
	core.proxyAlive.Store(2)

	core.reapChannel(core.browsers, &core.proxyAlive, time.Second)

	if core.proxyAlive.Load() != 1 {
		t.Fatalf("after reap, alive=%d, want 1", core.proxyAlive.Load())
	}
	if len(core.browsers) != 1 {
		t.Fatalf("after reap, idle=%d, want 1", len(core.browsers))
	}
	got := <-core.browsers
	if got != fresh {
		t.Fatal("expected the fresh browser to be kept, stale one reaped")
	}
}

func TestPoolGetDirectNotConfigured(t *testing.T) {
	p := newTestPool(1, 0, nil)
	_, err := p.GetDirect(context.Background())
	if err == nil || err.Error() != "direct browsers not configured" {
		t.Fatalf("GetDirect() error = %v, want 'direct browsers not configured'", err)
	}
}

func TestPoolGetDirectCreatesDirectBrowser(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(0, 1, f.new)

	b, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.useProxy {
		t.Fatal("GetDirect returned a proxy browser")
	}
	if f.count() != 1 || f.calls[0] {
		t.Fatalf("created useProxy=%v, want [false]", f.calls)
	}
}

func TestPoolResize(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(1, 0, f.new)

	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Resize(2, 0); err != nil {
		t.Fatal(err)
	}
	newCore := p.active.Load()
	if cap(newCore.browsers) != 2 {
		t.Fatalf("new core size=%d, want 2", cap(newCore.browsers))
	}

	// 归还旧浏览器 → 旧 core 已排空，直接回收
	if err := p.Put(b); err != nil {
		t.Fatal(err)
	}

	b2, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b2 == nil {
		t.Fatal("got nil browser from new core")
	}
}

func TestPoolGetWokenOnRetire(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(1, 0, f.new)

	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}

	type result struct {
		b   *Browser
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := p.Get(context.Background())
		ch <- result{b, err}
	}()

	oldCore := p.active.Load()
	waitFor(t, func() bool {
		oldCore.mu.Lock()
		defer oldCore.mu.Unlock()
		return oldCore.refs == 1
	}, "blocked Get to acquire ref")

	// 扩容 → 旧核退休，唤醒阻塞的 Get 重试新核
	if err := p.Resize(2, 0); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("blocked Get error = %v", r.err)
		}
		if r.b == nil {
			t.Fatal("blocked Get got nil browser")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Get was not woken by resize")
	}
}

func TestPoolSetHeadersAndMaxIdle(t *testing.T) {
	p := &Pool{}
	initial := map[string]string{"User-Agent": "ua-1"}
	p.headers.Store(&initial)
	p.maxIdle.Store(int64(time.Second))

	p.SetHeaders(map[string]string{"User-Agent": "ua-2", "X-Test": "v"})
	got := p.headersSnapshot()
	if got["User-Agent"] != "ua-2" || got["X-Test"] != "v" {
		t.Fatalf("headers = %+v", got)
	}
	// 原 map 不应被修改（copy-on-write）
	if initial["User-Agent"] != "ua-1" {
		t.Fatalf("original headers mutated: %+v", initial)
	}

	p.SetMaxIdleTime(3 * time.Second)
	if p.maxIdleDuration() != 3*time.Second {
		t.Fatalf("maxIdle = %v", p.maxIdleDuration())
	}
	p.Close()
}

func TestNewPoolRejectsNegativeSize(t *testing.T) {
	if _, err := NewPool(PoolConfig{Size: -1}); err == nil {
		t.Fatal("expected error for negative size")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
