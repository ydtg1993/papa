package browser

import (
	"context"
	"testing"
	"time"
)

func newTestPool(size, directSize int) (*Pool, error) {
	p := &Pool{cfg: PoolConfig{Size: size, DirectSize: directSize}}
	p.newBrowserFn = func(useProxy bool) (*Browser, error) {
		return &Browser{useProxy: useProxy}, nil
	}
	h := map[string]string{}
	p.headers.Store(&h)
	core, err := p.newCore(size, directSize)
	if err != nil {
		return nil, err
	}
	p.active.Store(core)
	return p, nil
}

func TestPoolGetDirectNotConfigured(t *testing.T) {
	p, err := newTestPool(1, 0)
	if err != nil {
		t.Fatalf("newTestPool: %v", err)
	}
	_, err = p.GetDirect(context.Background())
	if err == nil || err.Error() != "direct browsers not configured" {
		t.Fatalf("GetDirect() error = %v, want 'direct browsers not configured'", err)
	}
}

func TestPoolGetDirectRecreatesDirectBrowser(t *testing.T) {
	var createdProxy []bool
	p := &Pool{cfg: PoolConfig{DirectSize: 1}}
	p.newBrowserFn = func(useProxy bool) (*Browser, error) {
		createdProxy = append(createdProxy, useProxy)
		return &Browser{useProxy: useProxy}, nil
	}

	core := &poolCore{pool: p, browsers: make(chan *Browser, 1), direct: make(chan *Browser, 1)}
	core.direct <- &Browser{useProxy: false} // 死实例（Browser 为 nil）
	p.active.Store(core)

	b, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatalf("GetDirect() error = %v", err)
	}
	if b.useProxy {
		t.Fatal("GetDirect() returned a proxy browser")
	}
	if len(createdProxy) != 1 || createdProxy[0] {
		t.Fatalf("recreated browser useProxy = %v, want [false]", createdProxy)
	}
}

func TestPoolPutRoutesToCorrectChannel(t *testing.T) {
	p := &Pool{}
	p.newBrowserFn = func(useProxy bool) (*Browser, error) {
		return &Browser{useProxy: useProxy}, nil
	}
	core := &poolCore{pool: p, browsers: make(chan *Browser, 1), direct: make(chan *Browser, 1)}
	p.active.Store(core)

	// 直连浏览器归还到 direct
	if err := p.Put(&Browser{useProxy: false, core: core}); err != nil {
		t.Fatalf("Put(direct) error = %v", err)
	}
	select {
	case b := <-core.direct:
		if b.useProxy {
			t.Fatal("direct channel got a proxy browser")
		}
	default:
		t.Fatal("direct browser not routed to direct channel")
	}

	// 代理浏览器归还到 browsers
	if err := p.Put(&Browser{useProxy: true, core: core}); err != nil {
		t.Fatalf("Put(proxy) error = %v", err)
	}
	select {
	case b := <-core.browsers:
		if !b.useProxy {
			t.Fatal("proxy channel got a direct browser")
		}
	default:
		t.Fatal("proxy browser not routed to proxy channel")
	}
}

func TestPoolResize(t *testing.T) {
	p := &Pool{cfg: PoolConfig{Size: 1, DirectSize: 0}}
	p.newBrowserFn = func(useProxy bool) (*Browser, error) {
		return &Browser{useProxy: useProxy}, nil
	}
	core, err := p.newCore(1, 0)
	if err != nil {
		t.Fatalf("newCore: %v", err)
	}
	p.active.Store(core)

	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b == nil {
		t.Fatal("got nil browser")
	}

	if err := p.Resize(2, 0); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	newCore := p.active.Load()
	if newCore == core {
		t.Fatal("active core not swapped")
	}
	if cap(newCore.browsers) != 2 {
		t.Fatalf("new core size = %d, want 2", cap(newCore.browsers))
	}

	// 归还旧浏览器 → 旧 core 排空回收
	if err := p.Put(b); err != nil {
		t.Fatalf("Put old browser: %v", err)
	}

	// 新 core 仍可借出
	b2, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get from new core: %v", err)
	}
	if b2 == nil {
		t.Fatal("got nil browser from new core")
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
}

func TestPoolGetWokenOnResize(t *testing.T) {
	p := &Pool{cfg: PoolConfig{Size: 1, DirectSize: 0}}
	p.newBrowserFn = func(useProxy bool) (*Browser, error) {
		return &Browser{useProxy: useProxy}, nil
	}
	core, err := p.newCore(1, 0)
	if err != nil {
		t.Fatalf("newCore: %v", err)
	}
	p.active.Store(core)

	// 取走唯一浏览器，使通道为空
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// 再取一个会阻塞在旧核的空通道上
	type result struct {
		b   *Browser
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := p.Get(context.Background())
		ch <- result{b, err}
	}()

	// 等待 goroutine 阻塞（refs 变为 1）
	waitFor(t, func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return core.refs == 1
	}, "blocked Get to acquire ref")

	// 扩容 → 唤醒阻塞的 Get，重试新核
	if err := p.Resize(2, 0); err != nil {
		t.Fatalf("Resize: %v", err)
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
