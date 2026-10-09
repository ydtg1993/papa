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
	"github.com/ydtg1993/papa/v3/pkg/middleware/proxy"
)

// Pool Browser池封装管理多个rod。
// 浏览器按需懒创建：Get 时若无空闲实例且未达容量上限才新建；归还后不再立即重建。
// 空闲超过 max_idle_time 的实例由后台回收协程定期关闭，避免长期无任务仍反复唤起浏览器。
// Size/DirectSize 是并发上限（按需创建），不是常驻实例数，运行期不可调整。
// headers/max_idle_time 为共享可热更字段，改动即时全局生效。
type Pool struct {
	cfg          PoolConfig
	newBrowserFn func(useProxy bool) (*Browser, error) // 浏览器工厂，测试可注入

	browsers chan *Browser // 空闲的代理浏览器
	direct   chan *Browser // 空闲的强制直连浏览器
	cond     *sync.Cond    // L: mu，广播唤醒阻塞中的 Get

	proxyAlive  atomic.Int64 // 当前存活（含借用中）的代理浏览器数
	directAlive atomic.Int64 // 当前存活（含借用中）的直连浏览器数

	mu sync.Mutex // 保护空闲通道的收发与关闭，见 get/put/reapChannel/Close

	headers atomic.Pointer[map[string]string] // 共享可热更：默认请求头（copy-on-write）
	maxIdle atomic.Int64                      // 共享可热更：空闲回收阈值（纳秒）

	closed    atomic.Bool
	closeOnce sync.Once

	reaperMu   sync.Mutex
	reaperStop chan struct{} // 非 nil 表示空闲回收协程在运行
}

// PoolConfig 浏览器池配置
type PoolConfig struct {
	Size           int            // 代理浏览器并发上限（按需创建）
	DirectSize     int            // 强制直连浏览器并发上限（不经过代理）
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
	if cfg.Size < 0 || cfg.DirectSize < 0 {
		return nil, fmt.Errorf("invalid pool size: %d/%d", cfg.Size, cfg.DirectSize)
	}
	if cfg.Flags == nil {
		cfg.Flags = make(map[string]string)
	}
	if cfg.DefaultHeaders == nil {
		cfg.DefaultHeaders = make(map[string]string)
	}
	if cfg.DefaultCookies == nil {
		cfg.DefaultCookies = []*proto.NetworkCookieParam{}
	}

	// 只分配空闲通道，浏览器按需在 Get 时创建
	p := &Pool{
		cfg:      cfg,
		browsers: make(chan *Browser, cfg.Size),
		direct:   make(chan *Browser, cfg.DirectSize),
	}
	p.newBrowserFn = p.newBrowser
	p.cond = sync.NewCond(&p.mu)
	h := copyMap(cfg.DefaultHeaders)
	p.headers.Store(&h)
	p.maxIdle.Store(int64(cfg.MaxIdleTime))

	p.ensureReaper()
	return p, nil
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

	// 不用 MustConnect：连不上时它会 panic，而这里是在业务 goroutine 里取浏览器，
	// 该返回错误而不是把整个流程炸掉。连不上还要把刚拉起来的 Chrome 收掉，否则进程泄漏。
	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		l.Kill()
		return nil, fmt.Errorf("connect browser: %w", err)
	}
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
	if p.closed.Load() {
		return nil, fmt.Errorf("browser pool closed")
	}
	return p.get(ctx, p.browsers, &p.proxyAlive, true)
}

// GetDirect 从池中获取一个强制直连浏览器实例（阻塞直到有可用）
func (p *Pool) GetDirect(ctx context.Context) (*Browser, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("browser pool closed")
	}
	if cap(p.direct) == 0 {
		return nil, fmt.Errorf("direct browsers not configured")
	}
	return p.get(ctx, p.direct, &p.directAlive, false)
}

// get 从指定通道获取浏览器实例：优先复用空闲，其次按需新建，最后阻塞等待状态变化。
// 快路径（取空闲 / 新建）在锁外完成，避免慢操作（CDP 探测、Chrome 启动）持锁；
// 慢路径在 p.mu 下复查条件并用 cond.Wait 原子休眠，所有状态变化均持 p.mu 广播，杜绝丢失唤醒。
func (p *Pool) get(ctx context.Context, ch chan *Browser, alive *atomic.Int64, useProxy bool) (*Browser, error) {
	limit := int64(cap(ch))
	stopCtx := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer stopCtx()

	for {
		maxIdle := p.maxIdleDuration()

		// 1. 非阻塞复用空闲实例
		select {
		case b := <-ch:
			if b == nil {
				return nil, fmt.Errorf("browser pool closed")
			}
			if b = p.usable(b, alive, maxIdle); b == nil {
				// usable 关掉了一个死掉/超时空闲的实例并扣了 alive —— 那是"槽位空出来了"，
				// 得叫一声，否则可能有个已入睡的 Get 一直等不到人叫它。
				//
				// 触发很窄，写下来免得被当成多余代码删掉：实例是活的才能进池（put 会判 IsAlive），
				// 所以它只能在「put 广播 → 抢到的那个 Get 睡回去 → 实例随即死掉」这条缝里
				// 变成死实例；此时若接着这次新建又失败，槽位就一直空着，直到下一次 put / Close。
				p.broadcast()
				continue
			}
			b.markUsed()
			return b, nil
		default:
		}

		// 2. 未达容量上限则按需新建
		if limit > 0 && alive.Add(1) <= limit {
			b, err := p.newBrowserFn(useProxy)
			if err != nil {
				alive.Add(-1)
				return nil, fmt.Errorf("create browser: %w", err)
			}
			b.pool = p
			b.markUsed()
			return b, nil
		}
		if limit > 0 {
			alive.Add(-1)
		}

		// 3. 加锁复查条件，仍不满足则 cond.Wait（原子释放锁休眠）
		p.mu.Lock()
		if p.closed.Load() {
			p.mu.Unlock()
			return nil, fmt.Errorf("browser pool closed")
		}
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if len(ch) > 0 || (limit > 0 && alive.Load() < limit) {
			p.mu.Unlock()
			continue
		}
		p.cond.Wait()
		p.mu.Unlock()
	}
}

// usable 校验空闲实例：死掉或空闲超时的关闭并扣减存活计数，返回 nil 让调用方重试。
func (p *Pool) usable(b *Browser, alive *atomic.Int64, maxIdle time.Duration) *Browser {
	if !b.IsAlive() || (maxIdle > 0 && time.Since(b.GetLastUsed()) > maxIdle) {
		b.Close()
		alive.Add(-1)
		return nil
	}
	return b
}

// broadcast 唤醒所有阻塞在 get 里的调用方。
//
// 约定：**凡是会改变「还有没有空闲槽位」的状态变化，都要经它叫一声** ——
// 睡在 cond.Wait 上的 Get 只有被广播才会重新检查条件。
// 目前会改变条件的是：归还（put）、关闭池（Close）、空闲回收（reapChannel）、
// 以及取到死实例后把它关掉（get 的快路径，usable 扣掉 alive 那一下）。
func (p *Pool) broadcast() {
	p.mu.Lock()
	p.cond.Broadcast()
	p.mu.Unlock()
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
	if b.pool == nil {
		b.Close()
		return fmt.Errorf("browser does not belong to a pool")
	}
	return b.pool.put(b)
}

// put 归还浏览器：死实例直接回收不重建，池已关闭则直接回收。
func (p *Pool) put(b *Browser) error {
	enqueued := false
	if b.IsAlive() {
		b.markIdle()
		// 持锁发送：Close 也在同一把锁下 close(ch)，两者互斥，避免 send on closed channel
		p.mu.Lock()
		if !p.closed.Load() {
			ch := p.browsers
			if !b.useProxy {
				ch = p.direct
			}
			select {
			case ch <- b:
				enqueued = true
			default:
			}
		}
		p.mu.Unlock()
	}
	if !enqueued {
		b.Close()
		p.decAlive(b)
	}

	p.broadcast()
	return nil
}

// decAlive 扣减存活计数（实例已关闭）。
func (p *Pool) decAlive(b *Browser) {
	if b.useProxy {
		p.proxyAlive.Add(-1)
	} else {
		p.directAlive.Add(-1)
	}
}

// SetHeaders 运行期热更默认请求头（copy-on-write）。
func (p *Pool) SetHeaders(headers map[string]string) {
	h := copyMap(headers)
	p.headers.Store(&h)
}

// SetMaxIdleTime 运行期热更空闲回收阈值。
func (p *Pool) SetMaxIdleTime(d time.Duration) {
	p.maxIdle.Store(int64(d))
	p.ensureReaper()
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

// ensureReaper 空闲回收协程：max_idle_time > 0 时启动，定期关闭空闲超时的浏览器。
func (p *Pool) ensureReaper() {
	if p.closed.Load() || p.maxIdleDuration() <= 0 {
		return
	}
	p.reaperMu.Lock()
	defer p.reaperMu.Unlock()
	if p.reaperStop != nil {
		return
	}
	p.reaperStop = make(chan struct{})
	go p.reaperLoop(p.reaperStop)
}

func (p *Pool) reaperLoop(stop <-chan struct{}) {
	interval := p.reaperInterval()
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.reapIdle()
		}
	}
}

// reaperInterval 回收协程的扫描间隔，取 max_idle_time 的一半并夹在 [1s, 30s]。
func (p *Pool) reaperInterval() time.Duration {
	d := p.maxIdleDuration()
	if d <= 0 {
		return 0
	}
	d /= 2
	if d < time.Second {
		d = time.Second
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func (p *Pool) reapIdle() {
	maxIdle := p.maxIdleDuration()
	if maxIdle <= 0 {
		return
	}
	p.reapChannel(p.browsers, &p.proxyAlive, maxIdle)
	p.reapChannel(p.direct, &p.directAlive, maxIdle)
}

// reapChannel 排空空闲通道，关闭死掉/空闲超时的实例，放回仍有效的实例。
func (p *Pool) reapChannel(ch chan *Browser, alive *atomic.Int64, maxIdle time.Duration) {
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return
	}
	idle := make([]*Browser, 0, len(ch))
drain:
	for {
		select {
		case b := <-ch:
			idle = append(idle, b)
		default:
			break drain
		}
	}
	p.mu.Unlock()

	now := time.Now()
	fresh := idle[:0]
	for _, b := range idle {
		if !b.IsAlive() || now.Sub(b.GetLastUsed()) > maxIdle {
			b.Close()
			alive.Add(-1)
		} else {
			fresh = append(fresh, b)
		}
	}

	p.mu.Lock()
	if p.closed.Load() {
		for _, b := range fresh {
			b.Close()
			alive.Add(-1)
		}
	} else {
		for _, b := range fresh {
			select {
			case ch <- b:
			default:
				b.Close()
				alive.Add(-1)
			}
		}
	}
	if len(idle) > 0 {
		p.cond.Broadcast()
	}
	p.mu.Unlock()
}

// Close 关闭池中所有浏览器
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		p.reaperMu.Lock()
		if p.reaperStop != nil {
			close(p.reaperStop)
		}
		p.reaperMu.Unlock()

		// 持锁关通道（与 put/reapChannel 的发送互斥），再广播唤醒阻塞在 cond.Wait 的 Get；
		// 它们醒来后从已关闭的空通道收到 nil，返回 "browser pool closed" 而不是挂住。
		p.mu.Lock()
		close(p.browsers)
		close(p.direct)
		p.cond.Broadcast()
		p.mu.Unlock()

		for b := range p.browsers {
			b.Close()
		}
		for b := range p.direct {
			b.Close()
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
