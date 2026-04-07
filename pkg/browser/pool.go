package browser

import (
	"context"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/devices"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	_ "github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/pkg/middleware/proxy"
	"reflect"
	"sync"
	_ "sync/atomic"
	"time"
)

// Pool Browser池封装管理多个rod
type Pool struct {
	browsers  chan *Browser // 浏览器池（缓冲通道）
	mu        sync.Mutex
	closeOnce sync.Once
	closed    bool
	cfg       PoolConfig
}

// PoolConfig 浏览器池配置
type PoolConfig struct {
	Size           int            // 唤起浏览器操作实例
	MaxIdleTime    time.Duration  // 最大空闲时间，0 表示无限制
	ProxyManager   *proxy.Manager //代理管理器
	Headless       bool
	NoSandbox      bool
	BrowserPath    string            // 浏览器可执行文件路径，为空则使用系统默认
	Flags          map[string]string // 浏览器启动参数，如 "disable-gpu": "", "window-size": "1920,1080"
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

	pool := &Pool{
		browsers: make(chan *Browser, cfg.Size),
		cfg:      cfg,
	}
	// 预创建浏览器实例
	for i := 0; i < cfg.Size; i++ {
		browser, err := pool.newBrowser()
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("create browser %d: %w", i, err)
		}
		pool.browsers <- browser
	}
	return pool, nil
}

// newBrowser 创建一个新的浏览器实例
func (p *Pool) newBrowser() (*Browser, error) {
	l := launcher.New().
		Headless(p.cfg.Headless).
		NoSandbox(p.cfg.NoSandbox)
	// 如果配置了浏览器路径，则使用指定路径
	if p.cfg.BrowserPath != "" {
		l = l.Bin(p.cfg.BrowserPath)
	}
	// 设置自定义启动 flags
	for key, val := range p.cfg.Flags {
		if val == "" {
			l.Set(flags.Flag(key))
		} else {
			l.Set(flags.Flag(key), val)
		}
	}
	if false == reflect.ValueOf(p.cfg.ProxyManager).IsNil() {
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
		defaultDevice:  p.cfg.DefaultDevice,
		defaultHeaders: copyMap(p.cfg.DefaultHeaders),
		defaultCookies: copyCookies(p.cfg.DefaultCookies),
	}, nil
}

// Get 从池中获取一个浏览器实例（阻塞直到有可用）
func (p *Pool) Get(ctx context.Context) (*Browser, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("browser pool closed")
	}
	p.mu.Unlock()
	select {
	case b := <-p.browsers:
		if !b.IsAlive() {
			b.Close()
			newB, err := p.newBrowser()
			if err != nil {
				return nil, fmt.Errorf("failed to recreate dead browser: %v", err)
			}
			b = newB
		}
		b.markUsed()
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Put 将浏览器实例归还池中
func (p *Pool) Put(b *Browser) error {
	if b == nil {
		return fmt.Errorf("browser is nil")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		b.Close()
		return fmt.Errorf("browser pool closed")
	}
	p.mu.Unlock()
	// 检查浏览器是否存活  || 空闲超时检查
	if !b.IsAlive() || (p.cfg.MaxIdleTime > 0 && time.Since(b.lastUsed) > p.cfg.MaxIdleTime) {
		b.Close()
		newB, err := p.newBrowser()
		if err != nil {
			return fmt.Errorf("failed to recreate idle browser: %v", err)
		}
		b = newB
	}
	select {
	case p.browsers <- b:
	default:
		if b.IsAlive() {
			b.Close()
		}
	}
	return nil
}

// Close 关闭池中所有浏览器
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed = true
		close(p.browsers)
		for b := range p.browsers {
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
