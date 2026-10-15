package engine

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// `breaker_paused` **不跨重启**：暂停状态只活在内存里（引擎的暂停信号）。
//
// 这条以前靠"启动时把库里残留的 true 清掉"实现（那对列 + 两条 UPDATE），现在靠
// **启动按声明整块重建**（buildSiteStats）从结构上成立 —— 重建出来的快照一律是"没暂停"，
// 上一次留下的 true 没有回来的路。落点是同一份（模拟同一个库），引擎是新的（模拟重启）。
func TestSiteSnapshotResetsPauseStateOnRestart(t *testing.T) {
	store := &fakeSiteStore{}
	newEngine := func() *Engine {
		e := queueEngine(t, newFakeTaskDB(), workerpool.NewWorkerPool[*Task](1, 8, 1))
		e.SetSite(core.Site{Key: "a"})
		e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
		e.SetSiteStore(store)
		return e
	}

	first := newEngine()
	first.buildSiteStats()
	if !first.PauseSite("a", "人工暂停") {
		t.Fatal("暂停应当成功")
	}
	if !store.snapshot()[0].BreakerPaused {
		t.Fatalf("落点里该记着暂停：%+v", store.snapshot())
	}

	// "重启"：同一份落点、新的引擎
	second := newEngine()
	second.buildSiteStats()

	if st := second.GetSiteStats()["a"]; st.BreakerPaused || !st.BreakerPausedAt.IsZero() {
		t.Fatalf("重启后不该还显示「已暂停」：%+v", st)
	}
	if store.snapshot()[0].BreakerPaused {
		t.Fatalf("落点里那份也不该还留着上次的暂停态：%+v", store.snapshot())
	}
}

// 熔断**没启用**（`crawler.breaker.enabled: false`）时手动暂停某站：
// 后台必须看得见、也放行得了 —— HTTP 的「恢复抓取」端点 targets 取自 BreakerStatuses，
// 不列出来就等于"闸住了却既看不见也点不到恢复"，只能重启。
//
// 每把闸门**都列**（包括没启用、也没暂停的）：站点是必填维度，后台的逐站放行就靠这张表的键。
func TestBreakerStatusesListsDisabledSiteGate(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	// "没启用" 正是这条要测的场景（自动闸不了，但手动能闸）
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Window: time.Minute, Threshold: 1000})

	st, ok := e.BreakerStatuses()["a"]
	if !ok {
		t.Fatal("每把闸门都该列出来（否则那个站闸住了也点不到恢复）")
	}
	if st.Enabled || st.Paused || st.Site != "a" {
		t.Fatalf("没启用、没暂停的站应当是「关着、没暂停」且带上站点：%+v", st)
	}

	if !e.PauseCrawling("手工暂停") {
		t.Fatal("手动暂停应当成功（它不看 enabled）")
	}
	if st := e.BreakerStatuses()["a"]; !st.Paused {
		t.Fatalf("手动暂停过的站必须报成暂停：%+v", st)
	}

	if !e.ResumeCrawling() {
		t.Fatal("被手动暂停过就应当能放行")
	}
	if st := e.BreakerStatuses()["a"]; st.Paused {
		t.Fatalf("放行之后不该还是暂停：%+v", st)
	}

	// 没有"默认 scope"那一档了：站点是必填维度（声明要有 Key、任务要有 site）
	if _, ok := e.BreakerStatuses()[""]; ok {
		t.Fatal("空键不再是一个 scope")
	}
	if e.PauseSite("", "空键不是站点") {
		t.Fatal("暂停一个不存在的 scope 应当失败（后台据此回 400）")
	}
}

// 全局暂停/恢复也要落站点快照：单站 PauseSite/ResumeSite 会写（后台「已暂停」列读它），
// 全局 PauseCrawling/ResumeCrawling 只改内存信号的话，站点表在全局暂停后仍显示「运行中」、
// 恢复后也可能仍显示「已暂停」—— 两条路径行为不一致。
func TestPauseResumeCrawlingWritesSiteSnapshot(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	e.SetSiteBreaker("b", breaker.Config{Enabled: false, Threshold: 1})
	e.buildSiteStats()
	store := siteStoreOf(e)

	if !e.PauseCrawling("全局暂停") {
		t.Fatal("PauseCrawling 应当成功")
	}
	for _, row := range store.snapshot() {
		if !row.BreakerPaused {
			t.Fatalf("全局暂停后站点快照应显示「已暂停」：%+v", store.snapshot())
		}
	}

	if !e.ResumeCrawling() {
		t.Fatal("ResumeCrawling 应当成功")
	}
	for _, row := range store.snapshot() {
		if row.BreakerPaused || !row.BreakerPausedAt.IsZero() {
			t.Fatalf("全局恢复后站点快照应显示「未暂停」：%+v", store.snapshot())
		}
	}
}
