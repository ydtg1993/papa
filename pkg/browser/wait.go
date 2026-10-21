package browser

import (
	"fmt"
	"time"

	"github.com/go-rod/rod"
)

// WaitError 是「导航成功、但等待条件失败」时返回的错误。
//
// 为什么单独包一层：等待失败时最该看的是**那一刻的页面**（验证码 / 404 / 渲染到一半）。
// 页面在 NewPageWithOptions 的失败路径上会被关掉，上层（engine）拿不到 page 指针，
// 所以这里在关闭前尽力读一次 HTML（带短上限）随错误带出去，供归档与 trace 使用。
type WaitError struct {
	// Stage 卡在哪一步：selector / js / request-idle。
	Stage string
	// Err 底层错误（超时 / 元素不存在 / 表达式报错）。
	Err error
	// HTML 失败那一刻读到的页面内容；读不到时为空。
	HTML string
	// Elapsed 从开始等待到失败的时间（不含导航）。
	Elapsed time.Duration
}

func (e *WaitError) Error() string {
	return fmt.Sprintf("wait %s: %v", e.Stage, e.Err)
}

func (e *WaitError) Unwrap() error { return e.Err }

// waitHTMLTimeout 等待失败时读现场 HTML 的上限：页面卡死不能把错误路径拖成永久。
const waitHTMLTimeout = 3 * time.Second

// applyWaits 在导航 + WaitLoad 之后应用 PageOptions 的等待条件，顺序固定：
//
//	WaitSelector → WaitJS → WaitRequestIdle
//
// 三者共享 opts.Timeout 的**等待预算**（从进入本函数开始算；导航本身另有自己的超时）。
// 预算耗尽后每一步都会立刻超时，错误里点名是哪一步。
func applyWaits(page *rod.Page, opts PageOptions) error {
	if opts.WaitSelector == "" && opts.WaitJS == "" && opts.WaitRequestIdle <= 0 {
		return nil
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	start := time.Now()
	remaining := func() time.Duration {
		d := time.Until(deadline)
		if d <= 0 {
			return time.Nanosecond // 让 rod 立刻判超时，而不是传 0 变成"不限"
		}
		return d
	}
	fail := func(stage string, err error) error {
		html := ""
		if h, herr := page.Timeout(waitHTMLTimeout).HTML(); herr == nil {
			html = h
		}
		return &WaitError{Stage: stage, Err: err, HTML: html, Elapsed: time.Since(start)}
	}

	if opts.WaitSelector != "" {
		if _, err := page.Timeout(remaining()).Element(opts.WaitSelector); err != nil {
			return fail("selector", fmt.Errorf("selector %q: %w", opts.WaitSelector, err))
		}
	}
	if opts.WaitJS != "" {
		// rod 的 Wait 会把 JS 当**函数表达式**调用（内部 (js).apply(...)），
		// 而这里对外承诺的是"表达式" —— 包一层箭头函数，调用方写 `window.__ready === true` 即可。
		expr := "() => (" + opts.WaitJS + ")"
		if err := page.Timeout(remaining()).Wait(rod.Eval(expr)); err != nil {
			return fail("js", fmt.Errorf("expression %q: %w", opts.WaitJS, err))
		}
	}
	if opts.WaitRequestIdle > 0 {
		// WaitRequestIdle 返回的是等待函数，调用才会阻塞；耗时上限靠 page.Timeout 的 ctx。
		waitPage := page.Timeout(remaining())
		waitPage.WaitRequestIdle(opts.WaitRequestIdle, nil, nil, nil)()
		if err := waitPage.GetContext().Err(); err != nil {
			return fail("request-idle", fmt.Errorf("idle for %s: %w", opts.WaitRequestIdle, err))
		}
	}
	return nil
}
