package browser

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

// 容量 0 的池：Get 必须**立刻报错**，不能停在 cond.Wait 上永久阻塞。
//
// 回归点：get 的两条快路径（复用空闲 / 未达上限则新建）都带 `limit > 0` 前缀，容量 0 时全跳过
// → 停在 cond.Wait；而 ctx 取消那条退路要靠 AfterFunc 广播才唤醒，`context.Background()`
// 永不 cancel，于是死等 —— 调用方只看到"抓取挂死"，没有任何线索指向池容量。
// 生产路径到不了这里（config 校验层在 browser.enable=true 时已要求 pool_size > 0），
// 这条是给直接调 API 的人留的最后防线，与 GetDirect 对同类情况的处理对称。
func TestPoolGetNotConfigured(t *testing.T) {
	p := newTestPool(0, 0, nil)

	done := make(chan error, 1)
	go func() {
		_, err := p.Get(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("容量 0 时 Get 应当报错，而不是返回实例")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("容量 0 时 Get 挂在 cond.Wait 上了 —— 应当立刻报错")
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

// resolveBrowserBinary 的归一规则：留空按 browser_path 推断（有路径 = path，没有 = rod），
// 显式来源与路径冲突时拒绝。system 的真正探测依赖机器上装没装浏览器，不在这里测。
func TestResolveBrowserBinary(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		path       string
		wantSource string
		wantPath   string
		wantErr    bool
	}{
		{"留空且无路径 → rod", "", "", BrowserBinaryRod, "", false},
		{"留空有路径 → path", "", "/x/chrome", BrowserBinaryPath, "/x/chrome", false},
		{"rod", BrowserBinaryRod, "", BrowserBinaryRod, "", false},
		{"rod 带路径 → 拒绝", BrowserBinaryRod, "/x/chrome", "", "", true},
		{"system 带路径 → 拒绝", BrowserBinarySystem, "/x/chrome", "", "", true},
		{"path 无路径 → 拒绝", BrowserBinaryPath, "", "", "", true},
		{"path 有路径", BrowserBinaryPath, "/x/chrome", BrowserBinaryPath, "/x/chrome", false},
		{"未知来源 → 拒绝", "auto", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source, path, err := resolveBrowserBinary(c.source, c.path)
			if c.wantErr {
				if err == nil {
					t.Fatalf("应当报错，实得 source=%q path=%q", source, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if source != c.wantSource || path != c.wantPath {
				t.Fatalf("resolve(%q, %q) = (%q, %q)，want (%q, %q)",
					c.source, c.path, source, path, c.wantSource, c.wantPath)
			}
		})
	}
}

// 来源与路径冲突要在**建池时**就报错（引擎启动即失败），而不是等第一次抓取。
func TestNewPoolRejectsConflictingBinary(t *testing.T) {
	if _, err := NewPool(PoolConfig{Size: 1, BinarySource: BrowserBinaryRod, BrowserPath: "/x/chrome"}); err == nil {
		t.Fatal("rod + browser_path 应当拒绝")
	}
	if _, err := NewPool(PoolConfig{Size: 1, BinarySource: "auto"}); err == nil {
		t.Fatal("未知来源应当拒绝")
	}
}

// browser.flags 必须真的进 launcher 的命令行：用 FormatArgs 验证，不启动浏览器。
// 值的两种形态（空 = 布尔开关 / 非空 = --name=value）与前导 -- 的规范化都在这里钉住。
func TestLauncherFlags(t *testing.T) {
	p := &Pool{cfg: PoolConfig{
		Headless:  true,
		NoSandbox: true,
		Flags: map[string]string{
			"disable-gpu":            "",
			"--disable-extensions":   "",
			"renderer-process-limit": "2",
			"blink-settings":         "imagesEnabled=false",
		},
	}}
	joined := " " + strings.Join(p.newLauncher(false).FormatArgs(), " ") + " "
	for _, want := range []string{
		" --headless ",
		" --no-sandbox ",
		" --disable-gpu ",
		" --disable-extensions ", // 配置里带的前导 -- 被 rod 规范化掉，不会拼成 ----disable-extensions
		" --renderer-process-limit=2 ",
		" --blink-settings=imagesEnabled=false ",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("启动参数里缺 %q，实得：%s", want, joined)
		}
	}
}

// self_check 的失败要在**建池时**返回（引擎启动失败），而不是拖到第一次抓取。
// 用一个不存在的可执行文件：Launch 会立刻在 cmd.Start 失败，不会真起浏览器，也不碰网络。
func TestSelfCheckSurfacesLaunchError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-chrome")
	_, err := NewPool(PoolConfig{
		Size:         1,
		BinarySource: BrowserBinaryPath,
		BrowserPath:  missing,
		SelfCheck:    true,
	})
	if err == nil {
		t.Fatal("self_check 应当把启动失败返回给建池方")
	}
	if !strings.Contains(err.Error(), "browser self-check") {
		t.Fatalf("错误应点名 self-check，实得：%v", err)
	}
}

// WithBrowser 的回调式借用：成功、返回 error、panic 三条路径都要把实例还回池里。
func TestPoolWithBrowserReturnsInstance(t *testing.T) {
	p := newTestPool(1, 0, nil)
	called := 0
	if err := p.WithBrowser(context.Background(), func(b *Browser) error {
		called++
		if b == nil {
			t.Fatal("回调拿到 nil 浏览器")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithBrowser: %v", err)
	}
	if called != 1 {
		t.Fatalf("回调调用 %d 次，want 1", called)
	}
	// 实例已归还：再 Get 应当复用而不是新建
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("Get after WithBrowser: %v", err)
	}
	_ = p.Put(b)
}

func TestPoolWithBrowserReturnsOnError(t *testing.T) {
	p := newTestPool(1, 0, nil)
	want := errors.New("boom")
	if err := p.WithBrowser(context.Background(), func(*Browser) error { return want }); !errors.Is(err, want) {
		t.Fatalf("WithBrowser = %v, want %v", err, want)
	}
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("回调报错后实例没归还：%v", err)
	}
	_ = p.Put(b)
}

func TestPoolWithBrowserReturnsOnPanic(t *testing.T) {
	p := newTestPool(1, 0, nil)
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("回调 panic 应当继续向上传播")
			}
		}()
		_ = p.WithBrowser(context.Background(), func(*Browser) error { panic("boom-in-callback") })
	}()
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatalf("panic 后实例没归还：%v", err)
	}
	_ = p.Put(b)
}

// 直连版本走 direct 通道（useProxy=false）。
func TestPoolWithDirectBrowser(t *testing.T) {
	p := newTestPool(0, 1, nil)
	if err := p.WithDirectBrowser(context.Background(), func(b *Browser) error {
		if b.useProxy {
			t.Fatal("直连池给的实例应当 useProxy=false")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithDirectBrowser: %v", err)
	}
	b, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatalf("GetDirect after WithDirectBrowser: %v", err)
	}
	_ = p.Put(b)
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

// 创建失败释放槽位后必须广播：池满时另一个 Get 睡在 cond.Wait，若这次新建失败只扣 alive
// 不广播，等待者会一直睡到下一次 put / Close / 自己 ctx 取消（传不取消的 ctx 就是无限期阻塞）。
func TestPoolGetBroadcastsOnCreateFailure(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	factory := func(useProxy bool) (*Browser, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-release // 让 alive 停在 limit 上，好让另一个 Get 睡进 cond.Wait
			return nil, fmt.Errorf("boom")
		}
		return testBrowser(useProxy, true), nil
	}
	p := newTestPool(1, 0, factory)

	ch1 := make(chan error, 1)
	go func() { _, err := p.Get(context.Background()); ch1 <- err }()
	time.Sleep(20 * time.Millisecond) // 让 Get 1 加到 alive=limit 并卡在 factory 上

	type result struct {
		b   *Browser
		err error
	}
	ch2 := make(chan result, 1)
	go func() { b, err := p.Get(context.Background()); ch2 <- result{b, err} }()
	time.Sleep(20 * time.Millisecond) // 让 Get 2 走到 cond.Wait

	close(release) // Get 1 的创建失败 → 扣 alive + 广播

	select {
	case err := <-ch1:
		if err == nil {
			t.Fatal("Get 1 应返回创建错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Get 1 没返回")
	}

	select {
	case r := <-ch2:
		if r.err != nil {
			t.Fatalf("等待者应被唤醒并成功创建，实得 %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("创建失败释放槽位后，等待者应被广播唤醒（不修的话一直睡）")
	}
	p.Close()
}
