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
	p := &Pool{
		cfg:      PoolConfig{Size: size, DirectSize: directSize},
		browsers: make(chan *Browser, size),
		direct:   make(chan *Browser, directSize),
	}
	if factory != nil {
		p.newBrowserFn = factory
	} else {
		p.newBrowserFn = func(useProxy bool) (*Browser, error) { return testBrowser(useProxy, true), nil }
	}
	h := map[string]string{}
	p.headers.Store(&h)
	p.cond = sync.NewCond(&p.mu)
	return p
}

func TestPoolLazyCreation(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(2, 0, f.new)

	if f.count() != 0 {
		t.Fatalf("pool created %d browsers, want 0 (lazy)", f.count())
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

	stale := testBrowser(true, true)
	stale.lastUsed = time.Now().Add(-time.Minute)
	fresh := testBrowser(true, true)
	fresh.lastUsed = time.Now()

	p.browsers <- stale
	p.browsers <- fresh
	p.proxyAlive.Store(2)

	p.reapChannel(p.browsers, &p.proxyAlive, time.Second)

	if p.proxyAlive.Load() != 1 {
		t.Fatalf("after reap, alive=%d, want 1", p.proxyAlive.Load())
	}
	if len(p.browsers) != 1 {
		t.Fatalf("after reap, idle=%d, want 1", len(p.browsers))
	}
	got := <-p.browsers
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

// 容量已满时阻塞在 cond.Wait 的 Get，应被别人 Put 归还唤醒并复用那个实例（单池的唤醒路径）。
func TestPoolBlockedGetWakesOnPut(t *testing.T) {
	f := &countingFactory{}
	p := newTestPool(1, 0, f.new)

	b1, err := p.Get(context.Background())
	if err != nil {
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
	// 让第二个 Get 走到容量判断并睡到 cond 上（此后再归还才会走广播唤醒，而不是快路径取空闲）
	time.Sleep(20 * time.Millisecond)

	if err := p.Put(b1); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("blocked Get error = %v", r.err)
		}
		if r.b != b1 {
			t.Fatal("blocked Get should reuse the returned browser")
		}
		if f.count() != 1 {
			t.Fatalf("created %d browsers, want 1 (上限内复用，不得超建)", f.count())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Get was not woken by Put")
	}
	p.Close()
}

func TestPoolSetHeadersAndMaxIdle(t *testing.T) {
	p := newTestPool(1, 1, nil)
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

// Close 时必须唤醒阻塞在 cond.Wait 的 Get（返回错误而非永久挂住），且其后的 Put 不得 panic。
func TestPoolCloseWakesBlockedGet(t *testing.T) {
	p := newTestPool(1, 0, nil)
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := p.Get(context.Background())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond) // 让第二个 Get 睡到 cond 上

	p.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked Get should return an error after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Get was not woken by Close")
	}

	// Close 后归还：直接回收并报错，不能向已关闭通道发送
	if err := p.Put(b); err == nil {
		t.Fatal("Put after Close should return an error")
	}
}
