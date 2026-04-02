package browser

import (
	"context"
	"fmt"
	"github.com/chromedp/chromedp"
	"github.com/ydtg1993/papa/pkg/middleware/proxy"
	"sync"
	"sync/atomic"
	"time"
)

// Browser 封装一个 chromedp 浏览器实例
type Browser struct {
	AllocCtx  context.Context    // 分配器上下文（整个浏览器生命周期）
	Ctx       context.Context    // 当前标签页上下文（每次 Get 时新建）
	cancel    context.CancelFunc // 关闭浏览器的函数
	tabCancel context.CancelFunc // 关闭当前标签页的函数
	proxy     string             // 当前使用的代理地址
	useCount  int64              // 已使用次数（原子操作）
	lastUsed  time.Time          // 最后使用时间
	mu        sync.Mutex         // 保护 lastUsed
}

// Close 关闭浏览器
func (b *Browser) Close() {
	if b.tabCancel != nil {
		b.tabCancel()
	}
	if b.cancel != nil {
		b.cancel()
	}
}

// markUsed 标记浏览器被使用，增加使用计数并更新最后使用时间
func (b *Browser) markUsed() {
	atomic.AddInt64(&b.useCount, 1)
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()
}

// GetUseCount 获取使用次数
func (b *Browser) GetUseCount() int64 {
	return atomic.LoadInt64(&b.useCount)
}

// GetLastUsed 获取最后使用时间
func (b *Browser) GetLastUsed() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUsed
}

// Pool Browser池封装管理多个chromedp
type Pool struct {
	opts         []chromedp.ExecAllocatorOption // 公共选项（如 headless 等）
	browsers     chan *Browser                  // 浏览器池（缓冲通道）
	maxIdleTime  time.Duration                  // 最大空闲时间，0 表示无限制
	mu           sync.Mutex
	closeOnce    sync.Once
	closed       bool
	proxyManager *proxy.Manager
}

// NewPool 创建浏览器池
func NewPool(size int, commonOpts []chromedp.ExecAllocatorOption, maxIdleTime time.Duration, proxyMgr *proxy.Manager) (*Pool, error) {
	pool := &Pool{
		opts:         commonOpts,
		browsers:     make(chan *Browser, size),
		maxIdleTime:  maxIdleTime,
		proxyManager: proxyMgr,
	}

	// 预创建浏览器实例
	for i := 0; i < size; i++ {
		browser, err := pool.newBrowser()
		if err != nil {
			// 创建失败时关闭已创建的浏览器
			pool.Close()
			return nil, fmt.Errorf("create browser %d: %w", i, err)
		}
		pool.browsers <- browser
	}
	return pool, nil
}

// newBrowser 创建一个新的浏览器实例
func (p *Pool) newBrowser() (*Browser, error) {
	opts := make([]chromedp.ExecAllocatorOption, len(p.opts))
	copy(opts, p.opts)
	var proxyURL string
	if p.proxyManager != nil {
		// 获取代理（轮询）
		proxyURL = p.proxyManager.Next()
		if proxyURL != "" {
			opts = append(opts, chromedp.ProxyServer(proxyURL))
		}
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	b := &Browser{
		AllocCtx: allocCtx,
		cancel:   cancel,
		proxy:    proxyURL,
	}
	return b, nil
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
		if b == nil {
			return nil, fmt.Errorf("browser pool closed")
		}
		// 基于分配器创建新标签页
		tabCtx, tabCancel := chromedp.NewContext(b.AllocCtx)
		b.Ctx = tabCtx
		b.tabCancel = tabCancel
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
	// 1. 关闭当前标签页
	if b.tabCancel != nil {
		b.tabCancel()
		b.tabCancel = nil
	}
	b.Ctx = nil // 清空引用，避免误用
	// 2. 检查空闲超时，如果空闲太久则重建分配器
	if p.maxIdleTime > 0 && time.Since(b.lastUsed) > p.maxIdleTime {
		b.Close()
		newB, err := p.newBrowser()
		if err != nil {
			return fmt.Errorf("failed to recreate idle browser: %v", err)
		}
		b = newB
	}
	// 3. 归还到池中
	select {
	case p.browsers <- b:
	default:
		// 池已满，关闭此浏览器（正常情况下不会发生，因为我们使用缓冲通道大小等于池大小）
		b.Close()
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
