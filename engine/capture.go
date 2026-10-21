package engine

import (
	"context"
	"fmt"

	"github.com/ydtg1993/papa/v3/pkg/browser"
)

// captureTraceStep 捕获结果在 trace 里的步骤名。
// 成功与失败同名：抽屉里一眼看出"这一次尝试截请求"这件事，状态与 data 说明结果。
const captureTraceStep = "捕获网络请求"

// CaptureRequest 用浏览器打开 pageURL，截住第一条 opts.Match 命中的网络请求。
//
// 与直接调 pkg/browser 的差别（业务应优先用这个）：
//
//  1. 失败时把那一刻的页面 HTML 登记进归档（尝试结束时由「归档页面」步骤落盘，
//     OA 抽屉可直接下载）—— 浏览器层已经把 HTML 读进 CaptureResult.HTML，这里只转交；
//  2. 往本次尝试的 Trace 写结构化步骤：成功一条「捕获网络请求」，失败一条带
//     candidates / timeout 的失败步骤（见 docs 的 trace 约定表）；
//  3. 浏览器实例的生命周期交给池（WithBrowser），页面由浏览器层保证关闭。
//
// 逐请求头沿用渲染路径的同一套：ctx 上的站点级/逐请求头合并进 opts.Page；
// 显式 opts.Page.Headers 优先。与 FetchRendered 一致，逐请求代理地址
// （papa.WithProxyURL）在这里直接报错 —— Chrome 不支持逐请求改路由。
//
// trace 与归档都从 ctx 取（runAttempt 用 withTrace / withArchive 挂上）——
// 未开启时取出来的是 nil，方法照常干活、只少留痕。
func (e *Engine) CaptureRequest(ctx context.Context, pageURL string, opts browser.CaptureOptions) (*browser.CapturedRequest, error) {
	if err := browserProxyError(ctx); err != nil {
		return nil, err
	}
	if e.browserPool == nil {
		return nil, fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}

	// 逐请求头：ctx 上的站点级 / WithHeaders 合并进 PageOptions（与渲染路径同一套）；显式 opts.Page.Headers 优先。
	opts.Page = mergeBrowserOptions(ctx, opts.Page)

	var res *browser.CaptureResult
	err := e.browserPool.WithBrowser(ctx, func(b *browser.Browser) error {
		var cerr error
		res, cerr = b.CaptureRequest(ctx, pageURL, opts)
		return cerr
	})
	e.recordCapture(ctx, pageURL, res, err)

	if err != nil {
		return nil, err
	}
	if res == nil || res.Request == nil {
		return nil, fmt.Errorf("capture %s: no request captured", pageURL)
	}
	return res.Request, nil
}

// recordCapture 把一次捕获的结果写进 trace 与归档缓冲。两部分都是可选的：
// trace 未开启时 traceFromCtx 返回 nil（方法 no-op），归档未开启时 buffer 为 nil（方法 no-op）。
func (e *Engine) recordCapture(ctx context.Context, pageURL string, res *browser.CaptureResult, err error) {
	// 失败现场：页面 HTML 交给归档，尝试结束时按结局落盘。
	// 成功路径没有 HTML（命中即返回），这一段自然跳过。
	if res != nil && res.HTML != "" {
		archiveFromCtx(ctx).holdRendered(res.HTML, pageURL)
	}

	if err == nil && res != nil && res.Request != nil {
		traceFromCtx(ctx).Step(captureTraceStep, map[string]any{
			"matched_url":   res.Request.URL,
			"method":        res.Request.Method,
			"resource_type": string(res.Request.ResourceType),
			"elapsed_ms":    res.Elapsed.Milliseconds(),
		})
		return
	}
	if err == nil {
		// 理论上到不了：浏览器层成功时必带 Request；留一句能看懂的兜底。
		err = fmt.Errorf("capture %s: no request captured", pageURL)
	}

	tr := traceFromCtx(ctx)
	if tr == nil {
		return
	}
	data := map[string]any{"page_url": pageURL}
	if res != nil {
		data["elapsed_ms"] = res.Elapsed.Milliseconds()
		data["timeout_ms"] = res.Timeout.Milliseconds()
		data["candidates_total"] = res.SeenTotal
		candidates := make([]string, 0, len(res.Seen))
		for _, c := range res.Seen {
			candidates = append(candidates, c.Method+" "+c.URL)
		}
		data["candidates"] = candidates
	}
	tr.Fail(captureTraceStep, err, data)
}
