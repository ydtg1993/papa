package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/breaker"
)

// `breaker_paused` **不跨重启**：熔断闸门每次启动都是新建的、未暂停（暂停状态只活在内存里），
// 所以启动播种时库里残留的 true 要按"没暂停"算，并把那一对列清掉。
//
// 不清的后果：后台横幅与「站点」表一直显示"熔断已闸住"，而点「恢复」时闸门只会回
// "当前不在暂停态" —— 那个 true 自己永远清不掉（只有"先暂停再恢复"两下才能消）。
func TestSeedSiteRowsClearsStaleBreakerPaused(t *testing.T) {
	f := newFakeTaskDB()
	row := siteRow("a")
	row["breaker_paused"] = true
	row["breaker_paused_at"] = time.Now().Add(-time.Hour)
	f.siteRows = []fakeRow{row}

	e := siteEngine(t, f)
	e.seedSiteRows()

	st := e.GetSiteStats()["a"]
	if st.BreakerPaused || !st.BreakerPausedAt.IsZero() {
		t.Fatalf("启动后不该还显示「熔断已闸住」：%+v", st)
	}
	sql := f.written()
	if !strings.Contains(sql, "`breaker_paused`") {
		t.Fatalf("库里残留的 true 应当被清掉：\n%s", sql)
	}
	if !strings.Contains(sql, "`breaker_paused_at`") {
		t.Fatalf("与 ResumeSite 一致：解闸要把这一对列一起清（别留个孤零零的时间）：\n%s", sql)
	}
}

// 没暂停过的行不该被多写一次：启动播种只在库里真写着 true 时才动这一对列。
func TestSeedSiteRowsLeavesUnpausedRowsAlone(t *testing.T) {
	f := newFakeTaskDB()
	f.siteRows = []fakeRow{siteRow("a")} // breaker_paused = false
	e := siteEngine(t, f)
	e.seedSiteRows()

	for _, stmt := range strings.Split(f.written(), "\n") {
		if strings.HasPrefix(stmt, "UPDATE") && strings.Contains(stmt, "breaker_paused") {
			t.Fatalf("没暂停过的行不该被写 breaker_paused：\n%s", stmt)
		}
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
