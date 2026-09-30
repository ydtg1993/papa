package browser

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v2/pkg/middleware/proxy"
)

// Pool Browser池封装管理多个rod。
// 采用双核冷热切换：active 原子指针指向当前服务的 core；调整池大小时后台预建新 core 再原子换入，
// 旧 core 排空（在途浏览器归还后）自动回收。headers/max_idle_time 为共享可热更字段，改动即时全局生效。
type Pool struct {
	active       atomic.Pointer[poolCore]
	cfg          PoolConfig
	newBrowserFn func(useProxy bool) (*Browser, error) // 浏览器工厂，测试可注入

	headers atomic.Pointer[map[string]string] // 共享可热更：默认请求头（copy-on-write）
	maxIdle atomic.Int64                      // 共享可热更：空闲回收阈值（纳秒）

	closed    atomic.Bool
	closeOnce sync.Once
}

// poolCore 一个浏览器池内核，持有代理/直连两条通道与排空状态。
type poolCore struct {
	pool      *Pool
	browsers  chan *Browser
	direct    chan *Browser
	retiredCh chan struct{} // 内核被换下时关闭，用于唤醒阻塞中的 Get

	mu        sync.Mutex
	refs      int  // 在途数量：已借出未归还 + 阻塞在 Get 的调用
	retired   bool // 已被换下，排空中
	closeOnce sync.Once
}

// errRetired 内核已被换下，Get 应换到新内核重试。
var errRetired = fmt.Errorf("pool core retired")

// PoolConfig 浏览器池配置
type PoolConfig struct {
	Size           int            // 代理浏览器实例数
	DirectSize     int            // 强制直连浏览器实例数（不经过代理）
	MaxIdleTime    time.Duration  // 最大空闲时间，0 表示无限制
	ProxyManager   *proxy.Manager //代理管理器
	Headless       bool
	NoSandbox      bool
	Leakless       bool
	BrowserPath    string            // 浏览器可执行文件路径，为空则使用系统默认
	Flags          map[string]string // 浏览器启动参数
	DefaultDevice  *devices.Device   // 可选：全局设备模拟
	DefaultHeaders map[string]string // 默认 HTTP 请求头
	DefaultCookies []*proto.NetworkCookieParam
}

// NewPool 创建浏览器池
func NewPool(cfg PoolConfig) (*Pool, error) {
	if cfg.Flags == nil {
		cfg.Flags = make(map[string]string)
	}
	if cfg.DefaultHeaders == nil {
		cfg.DefaultHeaders = make(map[string]string)
	}
	if cfg.DefaultCookies == nil {
		cfg.DefaultCookies = []*proto.NetworkCookieParam{}
	}

	p := &Pool{cfg: cfg}
	p.newBrowserFn = p.newBrowser
	h := copyMap(cfg.DefaultHeaders)
	p.headers.Store(&h)
	p.maxIdle.Store(int64(cfg.MaxIdleTime))

	core, err := p.newCore(cfg.Size, cfg.DirectSize)
	if err != nil {
		return nil, err
	}
	p.active.Store(core)
	return p, nil
}

// newCore 预建一个内核：创建 size+directSize 个浏览器实例。
func (p *Pool) newCore(size, directSize int) (*poolCore, error) {
	c := &poolCore{
		pool:      p,
		browsers:  make(chan *Browser, size),
		direct:    make(chan *Browser, directSize),
		retiredCh: make(chan struct{}),
	}
	for i := 0; i < size; i++ {
		b, err := c.newBrowser(true)
		if err != nil {
			c.close()
			return nil, fmt.Errorf("create proxy browser %d: %w", i, err)
		}
		c.browsers <- b
	}
	for i := 0; i < directSize; i++ {
		b, err := c.newBrowser(false)
		if err != nil {
			c.close()
			return nil, fmt.Errorf("create direct browser %d: %w", i, err)
		}
		c.direct <- b
	}
	return c, nil
}

// newBrowser 通过工厂创建浏览器并绑定内核。
func (c *poolCore) newBrowser(useProxy bool) (*Browser, error) {
	b, err := c.pool.newBrowserFn(useProxy)
	if err != nil {
		return nil, err
	}
	b.core = c
	return b, nil
}

// newBrowser 创建一个新的浏览器实例，useProxy 为 false 时强制直连
func (p *Pool) newBrowser(useProxy bool) (*Browser, error) {
	l := launcher.New().
		Headless(p.cfg.Headless).
		NoSandbox(p.cfg.NoSandbox).
		Leakless(p.cfg.Leakless)
	if p.cfg.BrowserPath != "" {
		l = l.Bin(p.cfg.BrowserPath)
	}
	for key, val := range p.cfg.Flags {
		if val == "" {
			l.Set(flags.Flag(key))
		} else {
			l.Set(flags.Flag(key), val)
		}
	}
	if useProxy && p.cfg.ProxyManager != nil {
		if proxyURL := p.cfg.ProxyManager.Next(); proxyURL != "" {
			l.Proxy(proxyURL)
		}
	}

	url, err := l.Launch()
	if err != nil {
		return nil, err
	}

	browser := rod.New().ControlURL(url).MustConnect()
	return &Browser{
		Browser:        browser,
		launcher:       l,
		useProxy:       useProxy,
		defaultDevice:  p.cfg.DefaultDevice,
		defaultCookies: copyCookies(p.cfg.DefaultCookies),
	}, nil
}

// Get 从池中获取一个代理浏览器实例（阻塞直到有可用）
func (p *Pool) Get(ctx context.Context) (*Browser, error) {
	for {
		if p.closed.Load() {
			return nil, fmt.Errorf("browser pool closed")
		}
		core := p.active.Load()
		if core == nil {
			return nil, fmt.Errorf("browser pool closed")
		}
		b, err := core.get(ctx, core.browsers)
		if err == errRetired {
			continue
		}
		return b, err
	}
}

// GetDirect 从池中获取一个强制直连浏览器实例（阻塞直到有可用）
func (p *Pool) GetDirect(ctx context.Context) (*Browser, error) {
	for {
		if p.closed.Load() {
			return nil, fmt.Errorf("browser pool closed")
		}
		core := p.active.Load()
		if core == nil {
			return nil, fmt.Errorf("browser pool closed")
		}
		if cap(core.direct) == 0 {
			return nil, fmt.Errorf("direct browsers not configured")
		}
		b, err := core.get(ctx, core.direct)
		if err == errRetired {
			continue
		}
		return b, err
	}
}

// get 从指定通道获取浏览器实例，处理实例死亡重建
func (c *poolCore) get(ctx context.Context, ch chan *Browser) (*Browser, error) {
	c.mu.Lock()
	if c.retired {
		c.mu.Unlock()
		return nil, errRetired
	}
	c.refs++
	c.mu.Unlock()

	select {
	case b := <-ch:
		if b == nil {
			c.release()
			return nil, fmt.Errorf("browser pool closed")
		}
		if !b.IsAlive() {
			b.Close()
			newB, err := c.newBrowser(b.useProxy)
			if err != nil {
				c.release()
				return nil, fmt.Errorf("failed to recreate dead browser: %w", err)
			}
			b = newB
		}
		b.markUsed()
		return b, nil
	case <-c.retiredCh:
		c.release()
		return nil, errRetired
	case <-ctx.Done():
		c.release()
		return nil, ctx.Err()
	}
}

// Put 将浏览器实例归还池中
func (p *Pool) Put(b *Browser) error {
	if b == nil {
		return fmt.Errorf("browser is nil")
	}
	if p.closed.Load() {
		b.Close()
		return fmt.Errorf("browser pool closed")
	}
	if b.core == nil {
		b.Close()
		return fmt.Errorf("browser has no pool core")
	}
	return b.core.put(b)
}

// put 归还到浏览器所属内核；内核排空中则直接回收。
func (c *poolCore) put(b *Browser) error {
	defer c.release()

	c.mu.Lock()
	retired := c.retired
	c.mu.Unlock()
	if retired {
		b.Close()
		return nil
	}

	maxIdle := c.pool.maxIdleDuration()
	if !b.IsAlive() || (maxIdle > 0 && time.Since(b.GetLastUsed()) > maxIdle) {
		b.Close()
		newB, err := c.newBrowser(b.useProxy)
		if err != nil {
			return fmt.Errorf("failed to recreate idle browser: %w", err)
		}
		b = newB
	}
	ch := c.browsers
	if !b.useProxy {
		ch = c.direct
	}
	select {
	case ch <- b:
	default:
		if b.IsAlive() {
			b.Close()
		}
	}
	return nil
}

// Resize 运行期调整池大小：后台预建新内核再原子换入，旧内核排空后回收。
func (p *Pool) Resize(size, directSize int) error {
	if size < 0 || directSize < 0 {
		return fmt.Errorf("invalid pool size: %d/%d", size, directSize)
	}
	if p.closed.Load() {
		return fmt.Errorf("browser pool closed")
	}
	core, err := p.newCore(size, directSize)
	if err != nil {
		return err
	}
	old := p.active.Swap(core)
	if old != nil {
		old.retire()
	}
	return nil
}

// SetHeaders 运行期热更默认请求头（copy-on-write）。
func (p *Pool) SetHeaders(headers map[string]string) {
	h := copyMap(headers)
	p.headers.Store(&h)
}

// SetMaxIdleTime 运行期热更空闲回收阈值。
func (p *Pool) SetMaxIdleTime(d time.Duration) {
	p.maxIdle.Store(int64(d))
}

// headersSnapshot 返回当前共享默认请求头（只读，调用方不得修改）。
func (p *Pool) headersSnapshot() map[string]string {
	m := p.headers.Load()
	if m == nil {
		return nil
	}
	return *m
}

// maxIdleDuration 返回当前空闲回收阈值。
func (p *Pool) maxIdleDuration() time.Duration {
	return time.Duration(p.maxIdle.Load())
}

// release 归还一个在途引用；内核已排空且无在途引用时关闭。
func (c *poolCore) release() {
	c.mu.Lock()
	c.refs--
	shouldClose := c.retired && c.refs == 0
	c.mu.Unlock()
	if shouldClose {
		c.close()
	}
}

// retire 标记内核已换下，唤醒阻塞中的 Get 去重试新内核；无在途引用时立即关闭。
func (c *poolCore) retire() {
	c.mu.Lock()
	already := c.retired
	c.retired = true
	shouldClose := c.refs == 0
	c.mu.Unlock()
	if !already {
		close(c.retiredCh)
	}
	if shouldClose {
		c.close()
	}
}

// close 关闭通道并回收其中所有浏览器。
func (c *poolCore) close() {
	c.closeOnce.Do(func() {
		close(c.browsers)
		close(c.direct)
		for b := range c.browsers {
			b.Close()
		}
		for b := range c.direct {
			b.Close()
		}
	})
}

// Close 关闭池中所有浏览器
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		core := p.active.Swap(nil)
		if core != nil {
			core.retire()
			core.close()
		}
	})
}

// 辅助函数：深拷贝 map
func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// 辅助函数：深拷贝 cookies
func copyCookies(cookies []*proto.NetworkCookieParam) []*proto.NetworkCookieParam {
	if cookies == nil {
		return []*proto.NetworkCookieParam{}
	}
	cp := make([]*proto.NetworkCookieParam, len(cookies))
	for i, c := range cookies {
		cpy := *c
		cp[i] = &cpy
	}
	return cp
}
