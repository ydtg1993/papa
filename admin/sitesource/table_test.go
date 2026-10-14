package sitesource

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/core"
)

// buildTable 走一遍组件注册，顺带验证声明能被 oao 校验通过。
func buildTable(t *testing.T) *oao.TableInfo {
	t.Helper()
	o, err := oao.New(oao.Config{Tables: []oao.Table{Table(snapshotOf("a"))}})
	if err != nil {
		t.Fatalf("oao.New rejected the declaration: %v", err)
	}
	ts := o.Tables()
	if len(ts) != 1 {
		t.Fatalf("tables = %d, want 1", len(ts))
	}
	return ts[0]
}

// 声明：主键是站点键、列名是库里的真实列名、只读（没有动作）。
func TestTableDeclaration(t *testing.T) {
	info := buildTable(t)

	if info.Key != "site" || info.Group != "数据" {
		t.Fatalf("key/group = %q/%q", info.Key, info.Group)
	}
	if info.IDField != "key" {
		t.Fatalf("主键字段应当是 key：%q", info.IDField)
	}
	if len(info.Actions) != 0 {
		t.Fatalf("站点表应当只读（没有动作）：%+v", info.Actions)
	}

	cols := make(map[string]oao.ColumnInfo, len(info.Columns))
	for _, c := range info.Columns {
		cols[c.Name] = c
	}
	if _, ok := cols["key"]; !ok {
		t.Errorf("缺主键列 key：%+v", info.Columns)
	}
	if c, ok := cols["stage_count"]; !ok || c.Kind != oao.KindNumber {
		t.Errorf("stage_count 列应为 KindNumber: %+v", cols["stage_count"])
	}
	// 只有 breaker_paused_at 这一列是时间。原来的 updated_at 列随 crawler_sites 表一起没了 ——
	// 快照是一整块 blob，只有"这一行什么时候被改过"（crawler_config 那行），没有"某一站什么时候变的"。
	if c, ok := cols["breaker_paused_at"]; !ok || c.Kind != oao.KindTime {
		t.Errorf("breaker_paused_at 列应为 KindTime: %+v", cols["breaker_paused_at"])
	}
	if _, ok := cols["updated_at"]; ok {
		t.Error("updated_at 列不该还在（快照按块存，没有逐站时间）")
	}
	if c, ok := cols["base_url"]; !ok || c.Render != oao.RenderLink {
		t.Errorf("base_url 应当是可以点开的链接：%+v", cols["base_url"])
	}

	// 筛选的 Kind 也要写：不写按字符串处理，前端就是自由文本框，输入 true/是 会被 MySQL
	// 隐式转成 0 —— 筛「是」返回「否」的那一半。
	byFilter := make(map[string]oao.FilterInfo, len(info.Filters))
	for _, f := range info.Filters {
		byFilter[f.Name] = f
	}
	if f, ok := byFilter["breaker_paused"]; !ok || f.Kind != oao.KindBool {
		t.Errorf("筛选 breaker_paused 应为 KindBool：%+v", f)
	}
}

// ---------- 内存源：筛选 / 搜索 / 排序 / 分页 ----------
//
// 走**真实的组件路由**（而不是手工拼 oao.Query —— 筛选与排序规则是它的未导出字段，
// 只有 HTTP 参数解析那条路能给出来），与 gormsource 的用例同一个套路。

// snapshotOf 造一份站点快照：keys 里的第一个站"停着"（带暂停时间），阶段数按顺序 1、2、3…
func snapshotOf(keys ...string) func() map[string]core.SiteStat {
	return func() map[string]core.SiteStat {
		out := make(map[string]core.SiteStat, len(keys))
		for i, k := range keys {
			out[k] = core.SiteStat{Key: k, BaseURL: "https://" + k + ".example/", StageCount: i + 1}
		}
		if len(keys) > 0 {
			st := out[keys[0]]
			st.BreakerPaused = true
			st.BreakerPausedAt = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			out[keys[0]] = st
		}
		return out
	}
}

type siteListResp struct {
	Total int64            `json:"total"`
	Rows  []map[string]any `json:"rows"`
}

func serveSites(t *testing.T, table oao.Table, query string) siteListResp {
	t.Helper()
	o, err := oao.New(oao.Config{Tables: []oao.Table{table}})
	if err != nil {
		t.Fatalf("oao.New: %v", err)
	}
	mux := http.NewServeMux()
	o.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/oao/site?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out siteListResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// 内存源的四种查询各一条：默认排序 / 筛选 / 搜索 / 排序 / 分页。
func TestMemSourceQueryBehaviour(t *testing.T) {
	table := Table(snapshotOf("beta", "alpha", "gamma")) // 顺序故意乱：beta 停着、阶段数 1/2/3

	got := serveSites(t, table, "")
	if got.Total != 3 || len(got.Rows) != 3 {
		t.Fatalf("默认应当三站全出：%+v", got)
	}
	if got.Rows[0]["key"] != "alpha" {
		t.Fatalf("默认应按站点键升序（= 声明里的 DefaultSort）：%+v", got.Rows)
	}

	// 筛选：已暂停（KindBool + OpEq）
	got = serveSites(t, table, "filter[breaker_paused]=true")
	if got.Total != 1 || got.Rows[0]["key"] != "beta" {
		t.Fatalf("「已暂停」筛选没生效：%+v", got)
	}
	got = serveSites(t, table, "filter[breaker_paused]=false")
	if got.Total != 2 {
		t.Fatalf("「未暂停」应当两站：%+v", got)
	}

	// 搜索：跨列（key 与 base_url）OR LIKE，大小写不敏感
	got = serveSites(t, table, "search=AMMa")
	if got.Total != 1 || got.Rows[0]["key"] != "gamma" {
		t.Fatalf("搜索应当命中 base_url 里的站名且大小写不敏感：%+v", got)
	}

	// 筛选：站点键 LIKE
	got = serveSites(t, table, "filter[key]=alp")
	if got.Total != 1 || got.Rows[0]["key"] != "alpha" {
		t.Fatalf("站点键 LIKE 没生效：%+v", got)
	}

	// 排序：阶段数倒序
	got = serveSites(t, table, "sort=-stage_count")
	if len(got.Rows) != 3 || got.Rows[0]["key"] != "gamma" {
		t.Fatalf("按阶段数倒序没生效：%+v", got.Rows)
	}

	// 分页：total 是"满足条件的总数"，不是本页条数
	got = serveSites(t, table, "size=2")
	if got.Total != 3 || len(got.Rows) != 2 {
		t.Fatalf("第一页应当 2 条、总数 3：%+v", got)
	}
	got = serveSites(t, table, "size=2&page=2")
	if got.Total != 3 || len(got.Rows) != 1 {
		t.Fatalf("第二页应当 1 条、总数仍是 3：%+v", got)
	}
	got = serveSites(t, table, "size=2&page=9") // 越界页：空，不是 panic
	if len(got.Rows) != 0 {
		t.Fatalf("越界页应当为空：%+v", got)
	}
}

// 没给快照（宿主没注入）时不 panic：空表。
func TestMemSourceWithoutSnapshot(t *testing.T) {
	got := serveSites(t, Table(nil), "")
	if got.Total != 0 || len(got.Rows) != 0 {
		t.Fatalf("没有快照时应当是空表：%+v", got)
	}
}
