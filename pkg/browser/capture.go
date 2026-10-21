package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"
)

const (
	// defaultCaptureTimeout 未配置 CaptureTimeout 时的等待上限。
	defaultCaptureTimeout = 30 * time.Second
	// captureHTMLTimeout 失败路径上读一次现场 HTML 的上限。
	// 页面卡死时不能把错误路径拖成永久 —— 读不到就留空，捕获失败本身仍然是结论。
	captureHTMLTimeout = 3 * time.Second
	// captureSeenLimit 失败现场里保留的未命中请求样本数。
	// 样本是"看形态"用的（域名/路径长什么样），不是审计全量；总数在 SeenTotal。
	captureSeenLimit = 20
)

// CaptureOptions 一次「打开页面并截住网络请求」的配置。
type CaptureOptions struct {
	// Page 复用现有页面准备：设备模拟、默认/额外请求头、Cookies、导航超时。
	Page PageOptions

	// Match 判断这条请求是不是要截的那条；返回 true 即结束捕获。
	// 必填 —— 空谓词会连 favicon 一起截，属于调用方错误，方法直接返回 error。
	// 注意主文档与重定向也会各产生一条 RequestWillBeSent（ResourceType=Document），
	// 谓词要按资源类型/路径把自己要的挑出来。
	Match func(req CapturedRequest) bool

	// CaptureTimeout 导航开始后等匹配请求的上限；<=0 用 30s。
	CaptureTimeout time.Duration

	// WaitLoad 命中后是否还等一次 WaitLoad 再返回。默认 false：命中即返回，
	// 由 page.Close 中止剩余导航；需要 DOM 已经加载完时再开。
	WaitLoad bool
}

// CapturedRequest 捕获到的那条请求（只读快照）。
type CapturedRequest struct {
	URL          string
	Method       string
	ResourceType proto.NetworkResourceType
}

// CaptureResult 一次捕获的完整结果 —— 成功与失败共用，失败时的现场全靠它。
type CaptureResult struct {
	// Request 命中那条；失败（超时/导航错误）时为 nil。
	Request *CapturedRequest
	// HTML 失败路径上尽力读到的页面 HTML（成功时为空 —— 命中即返回，页面已关）。
	// 给上层登记归档用：捕获超时时页面到底渲染成了什么（验证码 / 404 / 播放器改版），
	// 是排查的第一证据。读不到（页面已不可用）时留空，不算错误。
	HTML string
	// Seen 失败路径上见过的**未命中**请求样本（最多 captureSeenLimit 条，其余只计数）。
	// 用来区分「页面一个请求都没发」/「发了但域名换了」/「路径改版」三种死法。
	Seen      []CapturedRequest
	SeenTotal int
	// Timeout 本次生效的 CaptureTimeout（写进 trace 用）。
	Timeout time.Duration
	// Elapsed 从导航开始到返回（命中或超时）的耗时。
	Elapsed time.Duration
}

// CaptureRequest 打开 pageURL，截住第一条 Match 命中的网络请求。
//
// 生命周期：页面在本方法内创建、关闭（成功、失败、超时、ctx 取消、panic 都一样）；
// 浏览器实例由调用方借用（引擎层用 Pool.WithBrowser 保证归还）。
// 返回的 URL 是请求发出的原始绝对地址（查询串原样保留）。
//
// 失败时返回的 CaptureResult 仍带现场：HTML（尽力读到的页面）、Seen（未命中请求样本）
// 与 Elapsed —— 上层据此登记归档、写 trace，而不是只拿到一句错误。
func (b *Browser) CaptureRequest(ctx context.Context, pageURL string, opts CaptureOptions) (*CaptureResult, error) {
	if opts.Match == nil {
		return nil, fmt.Errorf("capture %s: Match is required", pageURL)
	}
	page, err := b.newPageBase(opts.Page)
	if err != nil {
		return nil, err
	}
	page = page.Context(ctx)
	defer func() { _ = page.Close() }()

	navTimeout := opts.Page.Timeout
	if navTimeout <= 0 {
		navTimeout = 30 * time.Second
	}
	captureTimeout := opts.CaptureTimeout
	if captureTimeout <= 0 {
		captureTimeout = defaultCaptureTimeout
	}

	captured := make(chan CapturedRequest, 1)
	var mu sync.Mutex
	seen := make([]CapturedRequest, 0, captureSeenLimit)
	seenTotal := 0

	// 监听必须在 Navigate 之前同步注册：rod 的 EachEvent 在返回前就完成了 Network 域的
	// 启用与事件订阅，返回的 wait() 只是消费循环。
	wait := page.EachEvent(func(e *proto.NetworkRequestWillBeSent) bool {
		req := CapturedRequest{URL: e.Request.URL, Method: e.Request.Method, ResourceType: e.Type}
		if opts.Match(req) {
			select {
			case captured <- req:
			default:
			}
			return true // 首条即退出监听；后面的请求不再需要
		}
		mu.Lock()
		seenTotal++
		if len(seen) < captureSeenLimit {
			seen = append(seen, req)
		}
		mu.Unlock()
		return false
	})
	go wait()

	snapshot := func() ([]CapturedRequest, int) {
		mu.Lock()
		defer mu.Unlock()
		return append([]CapturedRequest(nil), seen...), seenTotal
	}

	start := time.Now()
	// 失败收尾：先尽力读一次页面 HTML（带短上限），再带上样本与耗时。
	fail := func() *CaptureResult {
		html := ""
		if h, herr := page.Timeout(captureHTMLTimeout).HTML(); herr == nil {
			html = h
		}
		s, total := snapshot()
		return &CaptureResult{
			HTML:      html,
			Seen:      s,
			SeenTotal: total,
			Timeout:   captureTimeout,
			Elapsed:   time.Since(start),
		}
	}

	navErr := make(chan error, 1)
	go func() {
		// 带缓冲：命中提前返回后，这个 goroutine 不会被无人接收的发送卡住。
		navErr <- page.Timeout(navTimeout).Navigate(pageURL)
	}()

	timer := time.NewTimer(captureTimeout)
	defer timer.Stop()

	for {
		select {
		case req := <-captured:
			if opts.WaitLoad {
				// 捕获已经拿到；WaitLoad 失败不推翻结果（日志价值大于错误价值）。
				_ = page.Timeout(navTimeout).WaitLoad()
			}
			s, total := snapshot()
			return &CaptureResult{
				Request:   &req,
				Seen:      s,
				SeenTotal: total,
				Timeout:   captureTimeout,
				Elapsed:   time.Since(start),
			}, nil

		case err := <-navErr:
			if err == nil {
				// 导航完成不代表不会再有请求（XHR/媒体请求可能在 load 之后）——
				// 继续等到命中或 CaptureTimeout。
				continue
			}
			// 导航失败前可能刚好截到：优先返回捕获结果。
			select {
			case req := <-captured:
				s, total := snapshot()
				return &CaptureResult{
					Request:   &req,
					Seen:      s,
					SeenTotal: total,
					Timeout:   captureTimeout,
					Elapsed:   time.Since(start),
				}, nil
			default:
			}
			return fail(), fmt.Errorf("capture: navigate %s: %w", pageURL, err)

		case <-timer.C:
			return fail(), fmt.Errorf("capture: no matching request within %s on %s", captureTimeout, pageURL)

		case <-ctx.Done():
			return fail(), fmt.Errorf("capture: %w", ctx.Err())
		}
	}
}
