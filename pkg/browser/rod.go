package browser

import (
	"context"
	"fmt"
	"log"
	"net/textproto"
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
	useProxy bool               // 是否走代理（false 为强制直连）
	useCount int64              // 已使用次数（原子操作）
	lastUsed time.Time          // 最后使用时间
	mu       sync.Mutex
	pool     *Pool // 归属池：Put 路由 + 读取池共享的默认请求头
	// 从池配置中继承的默认值（固定，不可热更）
	defaultDevice  *devices.Device
	defaultCookies []*proto.NetworkCookieParam

	// aliveOverride 测试钩子：非 nil 时 IsAlive 直接返回其值，跳过真实 CDP 探测。
	// 仅同包测试使用，生产环境恒为 nil。
	aliveOverride *bool

	// cleanupTimeout 测试钩子：Close 里等 launcher 收尾的上限；<=0 用 browserCleanupTimeout。
	// 只有测试会设它（真等 5 秒的用例太慢），生产路径一律走零值。
	cleanupTimeout time.Duration
}

// browserCleanupTimeout Close 里等进程收尾的上限（可被 Browser.cleanupTimeout 覆盖，测试用）。
//
// 为什么必须有上限：rod 的 `Launcher.Cleanup()` 是 `<-l.exit` —— **等到 Chrome 进程真的退出才返回**，
// 而"进程活着但 CDP 不可达"（渲染进程卡死 / websocket 断）时 `Browser.Close()` 关不掉它，
// 这一等就是永久。偏偏这条路是在 **持池锁** 时被调的（`pool.reapChannel` 第二段）：一次永久阻塞 =
// 整个池（Put / Get / Close / Broadcast）死锁 + 优雅停机永不返回。
const browserCleanupTimeout = 5 * time.Second

// Close 关闭浏览器。顺序是**先杀进程、再有上限地等收尾**，不能反过来（见上面那个常量的注释）：
//
//	① `launcher.Kill()` —— rod 的 Cleanup 等的是"进程退出"，Kill 是让那个等待能结束的唯一手段；
//	   顺带杀掉整棵进程组（Chrome 的子进程），这正是回收想要的"这个实例彻底没了"。
//	   这一步也是让下面那句 `Browser.Close()` 能立刻返回的原因：进程没了，CDP 连接随即断开，
//	   那次 `BrowserClose` 调用只会报个错（被丢掉），不会挂在那里等对面应答。
//	② `launcher.Cleanup()` —— 进程死了它还会清 UserDataDir，正常是毫秒级；**但不无条件等**：
//	   PID 拿不到时 Kill 什么都不做（rod 里 `l.PID() == 0` 直接 return），Cleanup 就还是那个永久等待。
//	   到点就放弃等待、留一条日志：调用方可能正握着池锁，最坏留一个 goroutine，绝不把池与停机拖死。
func (b *Browser) Close() {
	if b.launcher != nil {
		b.launcher.Kill()
	}
	if b.Browser != nil {
		_ = b.Browser.Close()
	}
	if b.launcher == nil {
		return
	}
	timeout := b.cleanupTimeout
	if timeout <= 0 {
		timeout = browserCleanupTimeout
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.launcher.Cleanup()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("browser: launcher cleanup still running after %v, giving up waiting（进程可能仍在，PID 拿不到时 Kill 是空操作）", timeout)
	}
}

// markUsed 标记浏览器被使用，增加使用计数并更新最后使用时间
func (b *Browser) markUsed() {
	atomic.AddInt64(&b.useCount, 1)
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()
}

// markIdle 标记浏览器已归还池中，记录空闲起始时间。
// 空闲回收（max_idle_time）以此时间点为基准。
func (b *Browser) markIdle() {
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
	if b.aliveOverride != nil {
		return *b.aliveOverride
	}
	if b.Browser == nil {
		return false
	}
	_, err := proto.TargetGetTargets{}.Call(b.Browser)
	return err == nil
}

// PageOptions 创建页面时的可选配置
type PageOptions struct {
	Device  *devices.Device   // 设备模拟，覆盖默认值
	Headers map[string]string // 额外请求头，会合并默认头
	// RemoveHeaders 从最终请求头里删掉这些键（含池的默认头）——"这次不要那个头"用；
	// 单独一条是因为 rod 的默认头是在内部先合并的，光靠 Headers 给空值删不掉。
	RemoveHeaders []string
	// Block 本页的资源拦截（导航前生效）；nil = 不拦截。见 BlockOptions。
	Block   *BlockOptions
	Cookies []*proto.NetworkCookieParam // 额外 Cookies，会追加到默认 Cookies
	Timeout time.Duration               // 导航超时，默认 30 秒
	Wait    time.Duration               // FetchOnce：WaitLoad 后额外等待再抓取快照
	// WaitSelector 导航后等元素出现；空 = 不等。
	WaitSelector string
	// WaitJS 导航后轮询求值这段表达式直到 truthy；空 = 不等。
	WaitJS string
	// WaitRequestIdle 导航后等「连续这么久没有网络请求」（最少空闲时长，不是上限）。
	WaitRequestIdle time.Duration
	// FetchOnce 专用：是否截图 / 是否整页截图（配合 Screenshot）
	Screenshot bool
	FullPage   bool
	// FetchOnce 专用：导航与 Wait 结束后的自定义交互（CLI 的 --act / --show 用）。
	// 返回后 FetchOnce 才读取 HTML/标题/截图，因此结果反映交互后的页面状态。
	Interact func(ctx context.Context, page *rod.Page) error
}

// FetchResult 一次抓取的结果快照（供 CLI 调试排查使用）。
type FetchResult struct {
	FinalURL    string
	Status      int
	Title       string
	ContentType string
	HTML        string
	Screenshot  []byte // 仅当 opts.Screenshot 时非空
}

// NewPage 创建页面（自动应用默认配置）
func (b *Browser) NewPage(ctx context.Context, url string) (*rod.Page, error) {
	return b.NewPageWithOptions(ctx, url, PageOptions{})
}

// NewPageWithOptions 创建页面并应用自定义配置。
// 失败时**必须把页面收掉** —— 页面挂在池化浏览器上，漏一个就少一个标签页。
func (b *Browser) NewPageWithOptions(ctx context.Context, url string, opts PageOptions) (page *rod.Page, err error) {
	page, err = b.newPageBase(opts)
	if err != nil {
		return nil, err
	}
	// 失败路径要关掉的是**刚建出来的那个页面**：用局部变量接住原指针，别去看命名返回值 ——
	// 下面几条失败路径写的是 `return nil, ...`，那会先把 page 置成 nil，defer 里再判 `page != nil`
	// 就永远为假，页面（挂在池化浏览器上的一个标签页）就这么漏了；反过来不判 nil 直接 Close 又会
	// `(*rod.Page)(nil).Close()` nil 解引用 panic（rod.Close 不判 nil）。
	// 触发条件很普通：DNS 失败、连接被拒、导航超时（默认 30s）—— 反复失败会一直吃标签页。
	created := page // 上面已经挡住 err != nil，这里必非 nil
	defer func() {
		if err != nil {
			_ = created.Close()
		}
	}()

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
	// 渲染条件（selector / js / request-idle）按固定顺序等；失败时错误里带着现场 HTML
	if err := applyWaits(page, opts); err != nil {
		return nil, err
	}
	return page, nil
}

// FetchOnce 抓取单个页面并返回渲染后快照，用于 CLI 调试（papa rod / diff）。
// 相比 NewPageWithOptions，额外采集主文档响应状态码与 MIME 类型，并支持截图。
// 失败时同样要把页面收掉（见 NewPageWithOptions）。
func (b *Browser) FetchOnce(ctx context.Context, url string, opts PageOptions) (res *FetchResult, err error) {
	page, err := b.newPageBase(opts)
	if err != nil {
		return nil, err
	}
	// **成功也要关**：FetchOnce 不把 page 交出去（HTML/标题/截图都拷进 res 了），页面只在这个函数里用得到。
	// 只判 `err != nil` 会让每次**成功**的调用都留下一个标签页 —— CLI 里跑一次看不出来，
	// 业务把它当长跑抓取接口用时就是持续泄漏。顺带：下面 `go wait()` 那条 EachEvent 协程
	// 要等页面关闭才退出，不关它也跟着一起漏。
	defer func() {
		_ = page.Close()
	}()
	page = page.Context(ctx)

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	// 监听主文档响应，采集状态码与 MIME 类型（在导航前同步注册，避免与 Timeout 竞争 page 上下文）
	var navMu sync.Mutex
	var status int
	var contentType string
	wait := page.EachEvent(func(e *proto.NetworkResponseReceived) bool {
		if e.Type == proto.NetworkResourceTypeDocument {
			navMu.Lock()
			status = e.Response.Status
			contentType = e.Response.MIMEType
			navMu.Unlock()
		}
		return false
	})
	go wait()

	if err := page.Timeout(timeout).Navigate(url); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	if err := page.Timeout(timeout).WaitLoad(); err != nil {
		return nil, fmt.Errorf("wait load: %w", err)
	}
	// 渲染条件（selector / js / request-idle）按固定顺序等；失败时错误里带着现场 HTML
	if err := applyWaits(page, opts); err != nil {
		return nil, err
	}
	if opts.Wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(opts.Wait):
		}
	}

	if opts.Interact != nil {
		if err := opts.Interact(ctx, page); err != nil {
			return nil, fmt.Errorf("interact: %w", err)
		}
	}

	html, err := page.HTML()
	if err != nil {
		return nil, fmt.Errorf("read html: %w", err)
	}
	info, err := page.Info()
	if err != nil {
		return nil, fmt.Errorf("read page info: %w", err)
	}

	navMu.Lock()
	res = &FetchResult{
		FinalURL:    info.URL,
		Status:      status,
		Title:       info.Title,
		ContentType: contentType,
		HTML:        html,
	}
	navMu.Unlock()

	if opts.Screenshot {
		shot, err := page.Screenshot(opts.FullPage, nil)
		if err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		res.Screenshot = shot
	}
	return res, nil
}

// newPageBase 创建空白页面并应用设备模拟、请求头、Cookies（导航前的公共准备逻辑）。
// 任何一步失败都先把刚建的页面关掉再返回 —— 调用方拿到 nil 就没法关了。
func (b *Browser) newPageBase(opts PageOptions) (*rod.Page, error) {
	page, err := b.Browser.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, fmt.Errorf("open page: %w", err)
	}

	// 1. 设备模拟（优先使用 opts.Device，否则使用默认设备）
	if device := opts.Device; device != nil {
		if err := page.Emulate(*device); err != nil {
			_ = page.Close()
			return nil, fmt.Errorf("emulate device: %w", err)
		}
	} else if b.defaultDevice != nil {
		if err := page.Emulate(*b.defaultDevice); err != nil {
			_ = page.Close()
			return nil, fmt.Errorf("emulate device: %w", err)
		}
	}

	// 2. 合并请求头（默认头来自池的共享可热更配置）。
	// 键一律按 HTTP 语义规范化：header 名大小写不敏感，站点级默认头可能是 `Authorization`、
	// 逐请求覆盖/删除写的却是 `authorization`—— map 按精确键删的话删不掉，旧凭据照发。
	// 规范化后 set/remove 与默认头落到同一个键上，覆盖/删除才按大小写不敏感生效。
	headerArgs := mergeExtraHeaders(b.pool.headersSnapshot(), opts.Headers, opts.RemoveHeaders)
	if len(headerArgs) > 0 {
		if _, err := page.SetExtraHeaders(headerArgs); err != nil {
			_ = page.Close()
			return nil, fmt.Errorf("set extra headers: %w", err)
		}
	}

	// 3. 合并 Cookies
	cookies := make([]*proto.NetworkCookieParam, 0, len(b.defaultCookies)+len(opts.Cookies))
	cookies = append(cookies, b.defaultCookies...)
	cookies = append(cookies, opts.Cookies...)
	if len(cookies) > 0 {
		if err := page.SetCookies(cookies); err != nil {
			_ = page.Close()
			return nil, fmt.Errorf("set cookies: %w", err)
		}
	}

	// 4. 本页资源拦截（必须在导航前；见 BlockOptions 的两种机制）
	if err := applyBlock(page, opts.Block); err != nil {
		_ = page.Close()
		return nil, err
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

// WithPage 在浏览器上开一个空白页交给 fn，返回后保证关页。
// 默认设备 / 池共享请求头 / 默认 Cookies 由 newPageBase 应用（不导航）。
// fn panic 时同样先关页，再把 panic 抛回。
func (b *Browser) WithPage(ctx context.Context, fn func(*rod.Page) error) error {
	page, err := b.newPageBase(PageOptions{})
	if err != nil {
		return err
	}
	page = page.Context(ctx)
	defer func() { _ = page.Close() }()
	return fn(page)
}

// WithNavigatedPage 等于 NewPageWithOptions + 保证关页：导航失败也会关。
func (b *Browser) WithNavigatedPage(ctx context.Context, url string, opts PageOptions, fn func(*rod.Page) error) error {
	page, err := b.NewPageWithOptions(ctx, url, opts)
	if err != nil {
		return err
	}
	page = page.Context(ctx)
	defer func() { _ = page.Close() }()
	return fn(page)
}

// mergeExtraHeaders 把默认头 + 逐请求覆盖 + 逐请求删除合并成 CDP SetExtraHeaders 需要的
// key,value,key,value 交替切片。所有键按 HTTP 语义规范化（大小写不敏感）——
// 站点级默认头可能是 `Authorization`、覆盖/删除写的却是 `authorization`，map 按精确键删不掉，
// 规范化后才落到同一个键上。空值已在调用方拆进 remove，这里不处理。
func mergeExtraHeaders(defaults, override map[string]string, remove []string) []string {
	merged := make(map[string]string, len(defaults)+len(override))
	for k, v := range defaults {
		merged[textproto.CanonicalMIMEHeaderKey(k)] = v
	}
	for k, v := range override {
		merged[textproto.CanonicalMIMEHeaderKey(k)] = v
	}
	for _, k := range remove {
		delete(merged, textproto.CanonicalMIMEHeaderKey(k))
	}
	args := make([]string, 0, len(merged)*2)
	for k, v := range merged {
		args = append(args, k, v)
	}
	return args
}
