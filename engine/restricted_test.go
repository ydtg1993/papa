package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/core"
	"github.com/ydtg1993/papa/v2/pkg/htmlfetch"
)

// 站点自己的受限页文案由框架从 `task.Site` 取 —— 调用点不必在每个 fetch 处记着传一遍
// （那是"漏了不报错、只是静默失效"的路）。这条同时钉住 stage 的取值与错误分类。
func TestRestrictedErrorUsesSiteKeywords(t *testing.T) {
	srv := serveBody(t, `<html><body><h1>本站专属拦截话术</h1></body></html>`)
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	e.SetSite(core.Site{Key: "a", RestrictedKeywords: []string{"本站专属拦截话术"}})
	e.SetSite(core.Site{Key: "b"}) // 没声明词表的站

	page, err := e.FetchHTML(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchHTML = %v", err)
	}
	// 框架默认词表判不出这一页（那是我编的一句话）
	if reason := htmlfetch.RestrictedReason(page); reason != "" {
		t.Fatalf("前置条件不成立：默认词表不该命中，实得 %q", reason)
	}

	// stage 传空串 → 用 task.Stage
	err = e.RestrictedError(&Task{ID: 1, Stage: "catalog", Site: "a"}, page, "")
	if err == nil {
		t.Fatal("站点词表命中了却没报错")
	}
	if core.Retryable(err) {
		t.Fatal("受限页必须不可重试")
	}
	if kind := core.ErrorKind(err); kind != "access_restricted" {
		t.Fatalf("分类 = %q, want access_restricted", kind)
	}
	if !strings.Contains(err.Error(), "catalog") || !strings.Contains(err.Error(), "本站专属拦截话术") {
		t.Fatalf("消息里应当有阶段与命中的话术：%v", err)
	}

	// 显式给了 stage 就用它（同一个任务里抓的第二个页面，想知道是哪个页面出的问题）
	err = e.RestrictedError(&Task{ID: 1, Stage: "detail", Site: "a"}, page, "playback")
	if err == nil || !strings.Contains(err.Error(), "playback") {
		t.Fatalf("显式 stage 没生效：%v", err)
	}

	// 站点没声明词表 → 判不出来（默认词表也不命中这句）
	if err := e.RestrictedError(&Task{ID: 2, Stage: "catalog", Site: "b"}, page, ""); err != nil {
		t.Fatalf("该站的词表是空的，不该命中：%v", err)
	}
	// 任务连 Site 都没填（老行/手工投递）→ 同样只是退化成默认词表，不该 panic
	if err := e.RestrictedError(&Task{ID: 3, Stage: "catalog"}, page, ""); err != nil {
		t.Fatalf("没有 Site 时不该命中：%v", err)
	}
}

// 干净页面返回 nil；page 为 nil 也不 panic（调用点常常是 `if err := …; err != nil { return err }`）。
func TestRestrictedErrorOnCleanPage(t *testing.T) {
	srv := serveBody(t, `<html><body><h1>正常的剧集详情页</h1></body></html>`)
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	e.SetSite(core.Site{Key: "a", RestrictedKeywords: []string{"本站专属拦截话术"}})

	page, err := e.FetchHTML(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchHTML = %v", err)
	}
	if err := e.RestrictedError(&Task{ID: 1, Stage: "catalog", Site: "a"}, page, "catalog"); err != nil {
		t.Fatalf("干净页面不该报错：%v", err)
	}
	if err := e.RestrictedError(&Task{ID: 1, Stage: "catalog", Site: "a"}, nil, "catalog"); err != nil {
		t.Fatalf("page 为 nil 应当返回 nil：%v", err)
	}
}
