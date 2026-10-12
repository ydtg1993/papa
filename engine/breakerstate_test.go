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

// 熔断**没启用**（`crawler.breaker.enabled: false`）时手动暂停默认 scope：
// 后台必须看得见、也放行得了 —— HTTP 的「恢复抓取」端点 targets 取自 BreakerStatuses，
// 不列出来就等于"闸住了却既看不见也点不到恢复"，只能重启。
func TestBreakerStatusesListsManuallyPausedDefaultScope(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	// 手工拼的 Engine 没有熔断闸门（submitEngine 不建它）：按 stage_test.go 的写法补一把**没启用**的 ——
	// "没启用" 正是这条要测的场景（自动闸不了，但手动能闸）。
	e.breaker = breaker.New(breaker.Config{Enabled: false, Window: time.Minute, Threshold: 1000}, nil)
	if e.breaker.Enabled() {
		t.Fatal("这条测的是熔断关着时的场景")
	}
	if _, ok := e.BreakerStatuses()[""]; ok {
		t.Fatal("没暂停、也没启用时，默认 scope 不该出现在列表里")
	}

	if !e.PauseCrawling("手工暂停") {
		t.Fatal("手动暂停应当成功（它不看 enabled）")
	}
	st, ok := e.BreakerStatuses()[""]
	if !ok {
		t.Fatal("手动暂停过的默认 scope 必须列出来，否则后台既看不见也放行不了")
	}
	if !st.Paused || st.Site != "" {
		t.Fatalf("默认 scope 的状态不对：%+v", st)
	}

	if !e.ResumeCrawling() {
		t.Fatal("被手动暂停过就应当能放行")
	}
	if _, ok := e.BreakerStatuses()[""]; ok {
		t.Fatal("放行之后（且没启用）默认 scope 该从列表里消失")
	}
}
