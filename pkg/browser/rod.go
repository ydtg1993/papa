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
	"github.com/go-rod/rod/lib/proto"
)

// Browser 封装一个 rod实例
type Browser struct {
	Browser  *rod.Browser       // rod 浏览器对象
	launcher *launcher.Launcher // 启动器，用于关闭
	useCount int64              // 已使用次数（原子操作）
	lastUsed time.Time          // 最后使用时间
	mu       sync.Mutex
	// 从池配置中继承的默认值
	defaultDevice  *devices.Device
	defaultHeaders map[string]string
	defaultCookies []*proto.NetworkCookieParam
}

// Close 关闭浏览器
func (b *Browser) Close() {
	if b.Browser != nil {
		_ = b.Browser.Close()
	}
	if b.launcher != nil {
		b.launcher.Cleanup()
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

// IsAlive 检查浏览器是否可用
func (b *Browser) IsAlive() bool {
	if b.Browser == nil {
		return false
	}
	_, err := proto.TargetGetTargets{}.Call(b.Browser)
	return err == nil
}

// PageOptions 创建页面时的可选配置
type PageOptions struct {
	Device  *devices.Device             // 设备模拟，覆盖默认值
	Headers map[string]string           // 额外请求头，会合并默认头
	Cookies []*proto.NetworkCookieParam // 额外 Cookies，会追加到默认 Cookies
	Timeout time.Duration               // 导航超时，默认 30 秒
}

// NewPage 创建页面（自动应用默认配置）
func (b *Browser) NewPage(ctx context.Context, url string) (*rod.Page, error) {
	return b.NewPageWithOptions(ctx, url, PageOptions{})
}

// NewPageWithOptions 创建页面并应用自定义配置
func (b *Browser) NewPageWithOptions(ctx context.Context, url string, opts PageOptions) (*rod.Page, error) {
	page := b.Browser.MustPage("")

	// 1. 设备模拟（优先使用 opts.Device，否则使用默认设备）
	if opts.Device != nil {
		page.MustEmulate(*opts.Device)
	} else if b.defaultDevice != nil {
		page.MustEmulate(*b.defaultDevice)
	}

	// 2. 合并请求头
	headers := make(map[string]string)
	for k, v := range b.defaultHeaders {
		headers[k] = v
	}
	for k, v := range opts.Headers {
		headers[k] = v
	}
	if len(headers) > 0 {
		// SetExtraHeaders 接受一个字符串切片，元素按 key, value, key, value 的顺序交替
		headerArgs := make([]string, 0, len(headers)*2)
		for k, v := range headers {
			headerArgs = append(headerArgs, k, v)
		}
		if _, err := page.SetExtraHeaders(headerArgs); err != nil {
			return nil, fmt.Errorf("set extra headers: %w", err)
		}
	}

	// 3. 合并 Cookies
	cookies := make([]*proto.NetworkCookieParam, 0, len(b.defaultCookies)+len(opts.Cookies))
	cookies = append(cookies, b.defaultCookies...)
	cookies = append(cookies, opts.Cookies...)
	if len(cookies) > 0 {
		if err := page.SetCookies(cookies); err != nil {
			return nil, fmt.Errorf("set cookies: %w", err)
		}
	}

	// 4. 导航到目标 URL
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	// 使用 page.Context(ctx) 支持外部取消，Timeout 设置超时
	if err := page.Context(ctx).Timeout(timeout).Navigate(url); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	// 等待页面加载完成
	if err := page.Timeout(timeout).WaitLoad(); err != nil {
		return nil, fmt.Errorf("wait load: %w", err)
	}
	return page, nil
}

// MustNewPage 与 NewPage 相同，但 panic 错误
func (b *Browser) MustNewPage(ctx context.Context, url string) *rod.Page {
	page, err := b.NewPage(ctx, url)
	if err != nil {
		panic(err)
	}
	return page
}
