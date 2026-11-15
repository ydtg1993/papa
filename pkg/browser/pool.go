package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

// Pool Browser池封装管理多个rod。
// 浏览器按需懒创建：Get 时若无空闲实例且未达容量上限才新建；归还后不再立即重建。
// 空闲超过 max_idle_time 的实例由后台回收协程定期关闭，避免长期无任务仍反复唤起浏览器。
// Size/DirectSize 是并发上限（按需创建），不是常驻实例数，运行期可通过 ApplyLimits 调整。
// headers/max_idle_time 为共享可热更字段，改动即时全局生效。
type Pool struct {
	cfg          PoolConfig
	newBrowserFn func(useProxy bool) (*Browser, error) // 浏览器工厂，测试可注入

	browsers                      []*Browser
	direct                        []*Browser
	cond                          *sync.Cond
	proxyAlive                    atomic.Int64
	directAlive                   atomic.Int64
	mu                            sync.Mutex
	admissionCtx                  context.Context
	admissionCancel               context.CancelFunc
	policyCtx                     context.Context
	policyCancel                  context.CancelFunc
	instances                     map[*Browser]instanceState
	proxyTarget, directTarget     int
	proxyCreating, directCreating int
	proxyWaiting, directWaiting   int
	initialized                   bool

	headers atomic.Pointer[map[string]string] // 共享可热更：默认请求头（copy-on-write）
	maxIdle atomic.Int64                      // 共享可热更：空闲回收阈值（纳秒）

	closed     atomic.Bool
	closeOnce  sync.Once
	cleanupErr error // first completed cleanup failure; guarded by mu

	reaperMu   sync.Mutex
	reaperWake chan struct{}
	reaperStop chan struct{} // 非 nil 表示空闲回收协程在运行
}

// 浏览器二进制来源（对应 config.yaml 的 browser.binary）。
const (
	BrowserBinaryRod    = "rod"    // Rod 管理、缺了自动下载的 Chromium
	BrowserBinarySystem = "system" // 系统已安装的 Chrome / Chromium / Edge
	BrowserBinaryPath   = "path"   // BrowserPath 指定的可执行文件
)

// PoolConfig 浏览器池配置
type PoolConfig struct {
	diagnosticStartup bool           // selected only by NewDiagnosticPool
	Network           egress.Config  `mapstructure:"network"`
	TempDir           string         // Profile root; empty preserves Rod's OS temporary directory default.
	Size              int            // 代理浏览器并发上限（按需创建）
	DirectSize        int            // 强制直连浏览器并发上限（不经过代理）
	MaxIdleTime       time.Duration  // 最大空闲时间，0 表示无限制
	ProxyManager      *proxy.Manager //代理管理器
	Headless          bool
	NoSandbox         bool
	Leakless          bool
	// BinarySource 二进制来源：留空 / rod（默认，Rod 自动下载）/ system（探测系统已装的）/ path。
	// 留空时看 BrowserPath：给了就是 path，没给就是 rod。
	BinarySource   string
	BrowserPath    string            // 可执行文件路径；system 模式下由 NewPool 填入探测结果
	Flags          map[string]string // 额外 Chromium 启动参数（值为空 = 布尔开关）
	SelfCheck      bool              // 建池时真实启动一次（rod 模式会触发下载），失败即报错
	DefaultDevice  *devices.Device   // 可选：全局设备模拟
	DefaultHeaders map[string]string // 默认 HTTP 请求头
	DefaultCookies []*proto.NetworkCookieParam
}

// resolveBrowserBinary 把 source + path 归一成"要么给可执行文件路径，要么留空让 Rod 下载管理"。
//
// 留空 source 时按 path 推断：给了路径 = path，没给 = rod —— browser_path 本来的意思
// 就是"指定一个可执行文件"，不写它则交给 Rod。system 的探测**只在这里做一次**，
// 而不是每次 newBrowser 都去 LookPath（同一台机器上结果不会变，也没必要重复扫）。
func resolveBrowserBinary(source, path string) (string, string, error) {
	switch source {
	case "":
		if path != "" {
			return BrowserBinaryPath, path, nil
		}
		return BrowserBinaryRod, "", nil
	case BrowserBinaryRod:
		if path != "" {
			return "", "", fmt.Errorf("browser binary=rod 时不能再配 browser_path（实得 %q）：要指定可执行文件请用 binary=path", path)
		}
		return BrowserBinaryRod, "", nil
	case BrowserBinarySystem:
		if path != "" {
			return "", "", fmt.Errorf("browser binary=system 时不能再配 browser_path（实得 %q）：system 就是指系统已安装的浏览器", path)
		}
		found, ok := launcher.LookPath()
		if !ok {
			return "", "", fmt.Errorf("browser binary=system，但系统里没找到 Chrome / Chromium / Edge：" +
				"装一个，或改成 binary=rod（Rod 会自动下载并管理，不用手动装）")
		}
		return BrowserBinarySystem, found, nil
	case BrowserBinaryPath:
		if path == "" {
			return "", "", fmt.Errorf("browser binary=path 时必须给 browser_path")
		}
		return BrowserBinaryPath, path, nil
	default:
		return "", "", fmt.Errorf("invalid browser binary %q: must be rod / system / path", source)
	}
}

// NewPool 创建浏览器池
func NewPool(cfg PoolConfig) (*Pool, error) {
	return NewPoolContext(context.Background(), cfg)
}

// NewPoolContext propagates cancellation through startup/self-check. Successful
// pooled browsers have their own lifetime and remain reusable after it ends.
func NewPoolContext(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cfg.Network.Validate(); err != nil {
		return nil, err
	}
	cfg.Network.PrivateOrigins = append([]string(nil), cfg.Network.PrivateOrigins...)
	cfg.Network.AllowedOrigins = append([]string(nil), cfg.Network.AllowedOrigins...)
	cfg.Flags = copyMap(cfg.Flags)
	if cfg.TempDir != "" {
		for key := range cfg.Flags {
			if strings.TrimLeft(key, "-") == "user-data-dir" || strings.TrimLeft(key, "-") == "disk-cache-dir" {
				return nil, fmt.Errorf("browser.temp_dir conflicts with browser.flags.%s", key)
			}
		}
	}
	if cfg.Size < 0 || cfg.DirectSize < 0 {
		return nil, fmt.Errorf("invalid pool size: %d/%d", cfg.Size, cfg.DirectSize)
	}
	// 二进制来源在建池时就定下来：system 找不到浏览器属于配置错误，该在启动时报，
	// 而不是等某条任务第一次 Get 时才冒出来。
	source, bin, err := resolveBrowserBinary(cfg.BinarySource, cfg.BrowserPath)
	if err != nil {
		return nil, err
	}
	cfg.BinarySource, cfg.BrowserPath = source, bin
	root, err := PrepareTempDir(cfg.TempDir)
	if err != nil {
		return nil, err
	}
	cfg.TempDir = root
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
		cfg:         cfg,
		proxyTarget: cfg.Size, directTarget: cfg.DirectSize, initialized: true,
	}
	p.cond = sync.NewCond(&p.mu)
	h := copyMap(cfg.DefaultHeaders)
	p.headers.Store(&h)
	p.maxIdle.Store(int64(cfg.MaxIdleTime))

	// 自检：建池时真实启动一次浏览器再关掉。放在这里而不是第一次抓取，是为了让
	// 「Chromium 还没下载 / 缺系统库 / 路径写错」变成**启动失败** —— 服务器部署时
	// 起一次进程就知道浏览器这一环通不通（rod 模式会在这里触发下载）。
	// 用完就关是刻意的：自检只负责验证，不占直连池的槽位（要常驻实例由 Get 按需建）。
	if cfg.SelfCheck {
		// Self-check also owns a process until cleanup completes. Register its
		// temporary direct slot even when the operational direct capacity is zero.
		p.directCreating++
		p.directAlive.Add(1)
		b, err := p.createBrowser(ctx, false)
		p.finishCreation(false, b)
		if b != nil {
			go b.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*browserCleanupTimeout)
		cleanupErr := p.waitCleanup(cleanupCtx)
		cleanupCancel()
		if err != nil || cleanupErr != nil {
			p.stopAdmission()
			return nil, fmt.Errorf("browser self-check: %w", errors.Join(err, cleanupErr))
		}
	}

	p.ensureReaper()
	return p, nil
}

// newLauncher 按池配置构造 launcher（**不启动**）。
// 抽出来是为了让启动参数能在单测里用 FormatArgs 验证，不必真拉一个 Chromium。
func (p *Pool) newLauncher() *launcher.Launcher {
	l := launcher.New().
		Headless(p.cfg.Headless).
		NoSandbox(p.cfg.NoSandbox).
		Leakless(p.cfg.Leakless)
	if p.cfg.BrowserPath != "" {
		l = l.Bin(p.cfg.BrowserPath)
	}
	// flags 最后应用：同名的 headless / no-sandbox 等会被这里覆盖。
	// 这是有意的 —— 配置里明确写了的开关应当赢（文档也写了别重复配）。
	for key, val := range p.cfg.Flags {
		if val == "" {
			l.Set(flags.Flag(key))
		} else {
			l.Set(flags.Flag(key), val)
		}
	}
	return l
}

// newBrowser 创建一个新的浏览器实例，useProxy 为 false 时强制直连
func (p *Pool) createBrowser(ctx context.Context, useProxy bool) (*Browser, error) {
	if p.newBrowserFn != nil {
		return p.newBrowserFn(useProxy)
	}
	p.mu.Lock()
	if p.admissionCtx == nil {
		p.admissionCtx, p.admissionCancel = context.WithCancel(context.Background())
	}
	poolCtx := p.admissionCtx
	if p.closed.Load() {
		p.admissionCancel()
	}
	p.mu.Unlock()
	startup, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(poolCtx, cancel)
	defer stop()
	if p.cfg.diagnosticStartup {
		return p.newDiagnosticBrowser(startup, ctx, useProxy)
	}
	return p.newBrowser(startup, useProxy)
}

func (p *Pool) newBrowser(ctx context.Context, useProxy bool) (result *Browser, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := p.newLauncher().Context(ctx)
	upstream, observeProxy, proxyState, err := p.selectBrowserProxy(useProxy, l.Get(flags.Flag("proxy-server")))
	if err != nil {
		return nil, err
	}
	gateway, err := egress.NewProxy(p.cfg.Network, upstream)
	if err != nil {
		return nil, err
	}
	if observeProxy != nil {
		gateway.SetProxyObserver(observeProxy)
	}
	success := false
	defer func() {
		if !success {
			gateway.Close()
		}
	}()
	secureLauncher(l, gateway.URL)
	// Honor explicit fixed profile flags when temp_dir is not configured.
	// NewPool rejects these flags together with an explicit temp_dir.
	fixedProfile := false
	for key := range p.cfg.Flags {
		if strings.TrimLeft(key, "-") == "user-data-dir" {
			fixedProfile = true
		}
	}
	dir := ""
	if !fixedProfile {
		var err error
		dir, err = NewTempProfile(l, p.cfg.TempDir, "browser-")
		if err != nil {
			return nil, err
		}
	}
	var process *ownedLauncher
	defer func() {
		if !success {
			if process != nil {
				// Hand partial ownership back with the error. Get registers it
				// before Close, retaining its slot if cleanup is still pending.
				result = &Browser{launcher: process, useProxy: useProxy}
			} else if dir != "" {
				cleanupErr := os.RemoveAll(dir)
				p.recordCleanupError(cleanupErr)
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	process, url, err := launchOwned(ctx, l, dir)
	if err != nil {
		return nil, err
	}

	// 不用 MustConnect：连不上时它会 panic，而这里是在业务 goroutine 里取浏览器，
	// 该返回错误而不是把整个流程炸掉。连不上还要把刚拉起来的 Chrome 收掉，否则进程泄漏。
	// Bound TCP/WebSocket/CDP startup, but detach the established connection
	// from this borrower's context before handing it to the reusable pool.
	dialer := &diagnosticDialer{ctx: ctx}
	ws := &cdp.WebSocket{Dialer: dialer}
	defer func() {
		if !success && dialer.conn != nil {
			_ = dialer.conn.Close()
		}
		if dialer.stop != nil {
			dialer.stop()
		}
	}()
	if err := ws.Connect(ctx, url, nil); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("connect browser: %w", err)
	}
	client := &diagnosticCDP{client: cdp.New().Start(ws), startup: ctx}
	browser := rod.New().Client(client)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connect browser: %w", err)
	}
	if err := dialer.release(); err != nil {
		return nil, err
	}
	client.ready.Store(true)
	success = true
	return &Browser{
		proxyState:     proxyState,
		Browser:        browser,
		gateway:        gateway,
		launcher:       process,
		useProxy:       useProxy,
		defaultDevice:  p.cfg.DefaultDevice,
		defaultCookies: copyCookies(p.cfg.DefaultCookies),
	}, nil
}

// Get 从池中获取一个代理浏览器实例（阻塞直到有可用）
func (p *Pool) Get(ctx context.Context) (*Browser, error)       { return p.get(ctx, true) }
func (p *Pool) GetDirect(ctx context.Context) (*Browser, error) { return p.get(ctx, false) }

// WithBrowser 从代理池借一个实例执行 fn，返回后保证归还。
//
// 归还走 defer：fn 返回 error 或直接 panic 都会先 Put 再继续向上传播。
// Put 的错误被忽略 —— 与现有各调用点的 `defer func() { _ = pool.Put(b) }()` 一致：
// fn 的结果比归还失败更重要，池关闭时 Put 自己会把实例关掉。
func (p *Pool) WithBrowser(ctx context.Context, fn func(*Browser) error) error {
	return p.withBrowser(ctx, p.Get, fn)
}

// WithDirectBrowser 同上，走强制直连池。
func (p *Pool) WithDirectBrowser(ctx context.Context, fn func(*Browser) error) error {
	return p.withBrowser(ctx, p.GetDirect, fn)
}

// withBrowser 是 WithBrowser / WithDirectBrowser 的共用实现。
// get 必须是方法值（p.Get / p.GetDirect）—— 测试注入池子时也走同一条路。
func (p *Pool) withBrowser(ctx context.Context, get func(context.Context) (*Browser, error), fn func(*Browser) error) error {
	b, err := get(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = p.Put(b) }()
	return fn(b)
}

// ErrDirectDisabled means subsequent direct borrows are disabled by policy.
var ErrDirectDisabled = fmt.Errorf("direct browsers not configured")

func (p *Pool) initLocked() {
	if p.initialized {
		return
	}
	p.proxyTarget, p.directTarget = p.cfg.Size, p.cfg.DirectSize
	p.initialized = true
}
func (p *Pool) classLocked(proxy bool) (*[]*Browser, *atomic.Int64, *int, *int, int) {
	p.initLocked()
	if proxy {
		return &p.browsers, &p.proxyAlive, &p.proxyCreating, &p.proxyWaiting, p.proxyTarget
	}
	return &p.direct, &p.directAlive, &p.directCreating, &p.directWaiting, p.directTarget
}
func (p *Pool) get(ctx context.Context, proxy bool) (*Browser, error) {
	stop := context.AfterFunc(ctx, p.broadcast)
	defer stop()
	for {
		p.mu.Lock()
		idle, alive, creating, waiting, target := p.classLocked(proxy)
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		if p.closed.Load() {
			p.mu.Unlock()
			return nil, fmt.Errorf("browser pool closed")
		}
		if target == 0 {
			p.mu.Unlock()
			if !proxy {
				return nil, ErrDirectDisabled
			}
			return nil, fmt.Errorf("browser pool not configured（pool_size 为 0）")
		}
		var b *Browser
		if int(alive.Load()) <= target && len(*idle) > 0 {
			b = (*idle)[len(*idle)-1]
			*idle = (*idle)[:len(*idle)-1]
			p.instances[b] = instanceReserved
		} else if int(alive.Load()) < target {
			alive.Add(1)
			*creating++
		} else {
			*waiting++
			p.cond.Wait()
			*waiting--
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()
		maxIdle := p.maxIdleDuration()
		if b != nil && (!b.IsAliveContext(ctx) || (maxIdle > 0 && time.Since(b.GetLastUsed()) > maxIdle)) {
			b.Close()
			continue
		}
		release, err := p.acquireBrowserPermit(ctx, proxy)
		created := b == nil
		if b == nil {
			if err == nil {
				func() {
					defer func() {
						if v := recover(); v != nil {
							release()
							p.finishCreation(proxy, nil)
							panic(v)
						}
					}()
					b, err = p.createBrowser(ctx, proxy)
				}()
			}
			p.finishCreation(proxy, b)
		}
		if err != nil {
			if release != nil {
				release()
			}
			if b != nil {
				if created {
					b.Close()
				} else {
					_ = p.put(b)
				}
			}
			if err == errPolicyChanged {
				continue
			}
			return nil, err
		}
		if b == nil {
			release()
			return nil, fmt.Errorf("browser factory returned nil")
		}
		b.setBorrowRelease(release)
		p.mu.Lock()
		_, alive, _, _, target = p.classLocked(proxy)
		err = ctx.Err()
		closed := p.closed.Load()
		accepted := err == nil && !closed && target > 0 && int(alive.Load()) <= target && !b.closed.Load()
		if accepted {
			p.instances[b] = instanceBorrowed
		}
		p.mu.Unlock()
		if !accepted {
			b.Close()
			if err != nil {
				return nil, err
			}
			if closed {
				return nil, fmt.Errorf("browser pool closed")
			}
			continue
		}
		b.markUsed()
		if b.proxyState != nil {
			b.proxyState.reset()
		}
		return b, nil
	}
}
func (p *Pool) finishCreation(proxy bool, b *Browser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, alive, creating, _, _ := p.classLocked(proxy)
	*creating--
	if b == nil {
		alive.Add(-1)
	} else {
		b.pool = p
		if p.instances == nil {
			p.instances = make(map[*Browser]instanceState)
		}
		p.instances[b] = instanceReserved
	}
	p.cond.Broadcast()
}

// usable remains a small standalone health helper for unregistered instances.
func (p *Pool) usable(b *Browser, alive *atomic.Int64, maxIdle time.Duration) *Browser {
	if !b.IsAlive() || (maxIdle > 0 && time.Since(b.GetLastUsed()) > maxIdle) {
		b.Close()
		alive.Add(-1)
		return nil
	}
	return b
}
func (p *Pool) broadcast() { p.mu.Lock(); p.cond.Broadcast(); p.mu.Unlock() }
func (p *Pool) Put(b *Browser) error {
	if b == nil {
		return fmt.Errorf("browser is nil")
	}
	if b.pool == nil {
		b.Close()
		return fmt.Errorf("browser does not belong to a pool")
	}
	return b.pool.put(b)
}
func (p *Pool) put(b *Browser) error {
	p.mu.Lock()
	state, ok := p.instances[b]
	if !ok || (state != instanceBorrowed && state != instanceReserved) {
		p.mu.Unlock()
		return fmt.Errorf("browser already returned or closed")
	}
	p.instances[b] = instanceReturning
	p.mu.Unlock()
	b.releaseBorrow()
	healthy := b.IsAlive()
	p.mu.Lock()
	idle, alive, _, _, target := p.classLocked(b.useProxy)
	closed := p.closed.Load()
	if healthy && !closed && int(alive.Load()) <= target && !b.closed.Load() {
		b.markIdle()
		*idle = append(*idle, b)
		p.instances[b] = instanceIdle
		p.cond.Broadcast()
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	b.Close()
	if closed {
		return fmt.Errorf("browser pool closed")
	}
	return nil
}

// ApplyLimits publishes capacities without waiting for borrowed instances to drain.
// Closing instances continue to occupy their slots until cleanup completes.
func (p *Pool) ApplyLimits(size, direct int, idle time.Duration) error {
	if size <= 0 || direct < 0 || idle < 0 {
		return fmt.Errorf("invalid browser limits: %d/%d/%s", size, direct, idle)
	}
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return fmt.Errorf("browser pool closed")
	}
	p.initLocked()
	if p.policyCancel != nil {
		p.policyCancel()
		p.policyCtx = nil
		p.policyCancel = nil
	}
	p.proxyTarget = size
	p.directTarget = direct
	p.maxIdle.Store(int64(idle))
	retired := p.trimLocked(true)
	retired = append(retired, p.trimLocked(false)...)
	p.cond.Broadcast()
	p.mu.Unlock()
	for _, b := range retired {
		go b.Close()
	}
	p.ensureReaper()
	return nil
}
func (p *Pool) trimLocked(proxy bool) []*Browser {
	idle, alive, _, _, target := p.classLocked(proxy)
	excess := int(alive.Load()) - target
	// Already closing instances are already committed to retirement.
	for b, state := range p.instances {
		if b.useProxy == proxy && state == instanceClosing {
			excess--
		}
	}
	var retired []*Browser
	for excess > 0 && len(*idle) > 0 {
		b := (*idle)[len(*idle)-1]
		*idle = (*idle)[:len(*idle)-1]
		p.instances[b] = instanceClosing
		retired = append(retired, b)
		excess--
	}
	return retired
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
	p.reaperMu.Lock()
	defer p.reaperMu.Unlock()
	if p.closed.Load() {
		return
	}
	if p.reaperStop == nil {
		if p.maxIdleDuration() <= 0 {
			return
		}
		p.reaperStop = make(chan struct{})
		p.reaperWake = make(chan struct{}, 1)
		go p.reaperLoop(p.reaperStop)
	}
	select {
	case p.reaperWake <- struct{}{}:
	default:
	}
}
func (p *Pool) reaperLoop(stop <-chan struct{}) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		var tick <-chan time.Time
		if interval := p.reaperInterval(); interval > 0 {
			timer.Reset(interval)
			tick = timer.C
		}
		select {
		case <-stop:
			return
		case <-p.reaperWake:
		case <-tick:
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
	p.mu.Lock()
	maxIdle := p.maxIdleDuration()
	if p.closed.Load() || maxIdle <= 0 {
		p.mu.Unlock()
		return
	}
	var retired []*Browser
	for _, idle := range []*[]*Browser{&p.browsers, &p.direct} {
		fresh := (*idle)[:0]
		for _, b := range *idle {
			if b.closed.Load() || time.Since(b.GetLastUsed()) > maxIdle {
				p.instances[b] = instanceClosing
				retired = append(retired, b)
			} else {
				fresh = append(fresh, b)
			}
		}
		*idle = fresh
	}
	p.mu.Unlock()
	for _, b := range retired {
		b.Close()
	}
}

// Close stops admission and retires idle instances; borrowed work drains on Put.
func (p *Pool) Close() {
	for _, b := range p.stopAdmission() {
		b.Close()
	}
}

// stopAdmission never performs process I/O under the lifecycle gate. Shutdown
// can retire every instance in parallel while Close preserves borrowed work.
func (p *Pool) stopAdmission() (retired []*Browser) {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed.Store(true)
		if p.admissionCancel != nil {
			p.admissionCancel()
		}
		retired = append(p.browsers, p.direct...)
		p.browsers = nil
		p.direct = nil
		for _, b := range retired {
			p.instances[b] = instanceClosing
		}
		p.cond.Broadcast()
		p.mu.Unlock()
		p.reaperMu.Lock()
		if p.reaperStop != nil {
			close(p.reaperStop)
		}
		p.reaperMu.Unlock()
	})
	return retired
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

// copyCookies 逐个复制 cookies。NetworkCookieParam 全是标量字段，`*c` 就是值拷贝，
// 没有内部可变字段要额外深挖；nil 元素跳过（它没有语义，也不该让它 panic）。
func copyCookies(cookies []*proto.NetworkCookieParam) []*proto.NetworkCookieParam {
	if cookies == nil {
		return []*proto.NetworkCookieParam{}
	}
	cp := make([]*proto.NetworkCookieParam, 0, len(cookies))
	for _, c := range cookies {
		if c == nil {
			continue
		}
		cpy := *c
		cp = append(cp, &cpy)
	}
	return cp
}
