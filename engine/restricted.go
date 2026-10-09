package engine

import (
	"github.com/ydtg1993/papa/v2/core"
	"github.com/ydtg1993/papa/v2/pkg/htmlfetch"
)

// RestrictedError 判断这一页是不是"被反爬拦下了"：命中就返回标准的 access_restricted 错误
// （**不可重试**），干净返回 nil。
//
//	page, err := engine.FetchHTML(ctx, url)
//	if err != nil { return err }
//	if err := engine.RestrictedError(task, page, "catalog"); err != nil { return err }
//
// 本站的词表由框架从 `task.Site` 取 —— 不用自己 `engine.Site(task.Site)` 再传 `extra`。
// 那条路的问题是**漏了不报错**：站点声明里写了 `RestrictedKeywords`、调用点忘了传，
// 判定只是静默地不生效（每个 fetch 调用点都要记着传一遍，迟早会漏）。
//
// stage 传空串则用 task.Stage（同一个任务里抓的每个页面都算这个阶段；要看具体是哪个页面，
// 错误消息里本来就带着 URL）。
func (e *Engine) RestrictedError(task *Task, page *htmlfetch.Page, stage string) error {
	if page == nil {
		return nil
	}
	var keywords []string
	if task != nil {
		if site, ok := e.Site(task.Site); ok {
			keywords = site.RestrictedKeywords
		}
	}
	reason := htmlfetch.RestrictedReason(page, keywords...)
	if reason == "" {
		return nil
	}
	if stage == "" && task != nil {
		stage = task.Stage
	}
	pageURL := ""
	if page.URL != nil {
		pageURL = page.URL.String()
	}
	return core.RestrictedPageError(stage, pageURL, reason)
}
