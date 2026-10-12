package sitesource

import (
	"testing"

	"github.com/ydtg1993/oao"
)

// buildTable 走一遍组件注册，顺带验证声明能被 oao 校验通过。
func buildTable(t *testing.T) *oao.TableInfo {
	t.Helper()
	o, err := oao.New(oao.Config{Tables: []oao.Table{Table(nil)}})
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
	for _, f := range []string{"breaker_paused_at", "updated_at"} {
		if c, ok := cols[f]; !ok || c.Kind != oao.KindTime {
			t.Errorf("%s 列应为 KindTime: %+v", f, cols[f])
		}
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
