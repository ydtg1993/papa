package sitesource

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/core"
)

// 站点表格的数据源：**内存快照**，不走 SQL。
//
// 为什么：站点只有几行，而且每次刷新都是全量 —— 走一趟 SQL（分页/搜索/排序都由数据库做）
// 换不来任何东西，却要为此维护一张表（模型、迁移、两条写路径）。现在那份快照由引擎按声明重建、
// 暂停/恢复时整块回写（落点是 `crawler_config` 的 `sites` 键，见 admin/configstore），
// 页面直接渲染它。
//
// oao 的 Source 只有一个 List，接口注释里就写着"具体怎么落到 SQL / 接口 / **内存过滤**，
// 由 Source 自己决定"—— 这里就是内存过滤那一种。筛选/搜索/排序的语义**与 gormsource 对齐**
// （OpLike 前后通配、OpPrefix 只有后通配、OpEq 按 Kind 解析、解析不出来的条件**丢掉**而不是报错；
// 搜索是跨列的 OR LIKE，MySQL 的 ci 排序规则下大小写不敏感，这里也用大小写不敏感）。
type memSource struct {
	snapshot func() map[string]core.SiteStat
	// search 跨列搜索（与 gormsource.Config.Search 同一个角色）
	search []string
}

// Table 返回内置「站点」表格的声明。**只读**：站点快照是引擎按声明重建、暂停/恢复时整块回写的，
// 运营没有可写的列。
//
// snapshot 由宿主给（papa 传的是 `Engine.GetSiteStats` 的那份内存快照）。
func Table(snapshot func() map[string]core.SiteStat) oao.Table {
	return oao.Table{
		Key: "site", Label: "站点", Group: "数据",
		Source:  &memSource{snapshot: snapshot, search: []string{"key", "base_url"}},
		IDField: "key", // 主键用「站点键」而不是自增 ID：与熔断 scope 一脉相承
		Columns: []oao.Column{
			{Field: "key", Label: "站点键", NoEdit: true},
			{Field: "base_url", Label: "BaseURL", Render: oao.RenderLink, Href: "{base_url}", NewTab: true, NoEdit: true},
			{Field: "stage_count", Label: "阶段数", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
			{Field: "breaker_paused", Label: "已暂停", Kind: oao.KindBool, Width: "90px", NoEdit: true},
			{Field: "breaker_paused_at", Label: "暂停时间", Kind: oao.KindTime, Width: "150px", NoEdit: true},
		},
		Filters: []oao.Filter{
			{Field: "key", Label: "站点键", Op: oao.OpLike},
			{Field: "breaker_paused", Label: "已暂停", Kind: oao.KindBool, Op: oao.OpEq},
		},
		DefaultSort: "key",
		PageSize:    50,
	}
}

// List 实现 oao.Source：全量快照 → 筛选 → 搜索 → 排序 → 分页。
func (s *memSource) List(_ context.Context, q oao.Query) ([]map[string]any, int64, error) {
	all := s.rows()

	filtered := make([]core.SiteStat, 0, len(all))
	for _, r := range all {
		if s.matchSearch(r, q.Search) && s.matchFilters(r, q) {
			filtered = append(filtered, r)
		}
	}
	s.sortRows(filtered, q)

	total := int64(len(filtered))
	from, to := pageBounds(q.Page, q.Size, len(filtered))
	out := make([]map[string]any, 0, to-from)
	for _, r := range filtered[from:to] {
		out = append(out, rowOf(r))
	}
	return out, total, nil
}

// rows 取一份快照（按站点键排序，保证默认顺序稳定）。
func (s *memSource) rows() []core.SiteStat {
	if s.snapshot == nil {
		return nil
	}
	m := s.snapshot()
	out := make([]core.SiteStat, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// rowOf 把一行快照摊成表格组件要的 map（键用列声明的 Field 名）。
func rowOf(r core.SiteStat) map[string]any {
	row := map[string]any{
		"key":               r.Key,
		"base_url":          r.BaseURL,
		"stage_count":       r.StageCount,
		"breaker_paused":    r.BreakerPaused,
		"breaker_paused_at": r.BreakerPausedAt,
	}
	if r.BreakerPausedAt.IsZero() {
		// 零值时间渲染出来是 "0001-01-01"，比空单元格更难看也更没意义
		row["breaker_paused_at"] = time.Time{}
	}
	return row
}

// matchSearch 跨列 OR LIKE（大小写不敏感，与 MySQL 的 ci 排序规则一致）。
func (s *memSource) matchSearch(r core.SiteStat, kw string) bool {
	kw = strings.TrimSpace(kw)
	if kw == "" || len(s.search) == 0 {
		return true
	}
	needle := strings.ToLower(kw)
	for _, field := range s.search {
		switch field {
		case "key":
			if strings.Contains(strings.ToLower(r.Key), needle) {
				return true
			}
		case "base_url":
			if strings.Contains(strings.ToLower(r.BaseURL), needle) {
				return true
			}
		}
	}
	return false
}

// matchFilters 按声明过的筛选项逐个判。解析不出来的值**丢掉这个条件**（与 gormsource 同一取向：
// 不报错、也不当成"筛出空"）。
func (s *memSource) matchFilters(r core.SiteStat, q oao.Query) bool {
	for _, f := range q.Filters() {
		if f.Empty() {
			continue
		}
		switch f.Field() {
		case "key":
			if f.Op() == oao.OpPrefix {
				if !strings.HasPrefix(strings.ToLower(r.Key), strings.ToLower(f.String())) {
					return false
				}
				continue
			}
			// OpLike（声明里就是它）：前后通配
			if !strings.Contains(strings.ToLower(r.Key), strings.ToLower(f.Raw())) {
				return false
			}
		case "breaker_paused":
			if b, ok := f.Bool(); ok && b != r.BreakerPaused {
				return false
			}
		}
	}
	return true
}

// sortRows 按 q.SortFields() 排；没给（或字段都不认识）就按站点键升序（= 声明里的 DefaultSort）。
func (s *memSource) sortRows(rows []core.SiteStat, q oao.Query) {
	fields := q.SortFields()
	if len(fields) == 0 {
		return // rows 已经按 key 升序
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, f := range fields {
			var less, eq bool
			switch f.Field {
			case "key":
				less, eq = rows[i].Key < rows[j].Key, rows[i].Key == rows[j].Key
			case "base_url":
				less, eq = rows[i].BaseURL < rows[j].BaseURL, rows[i].BaseURL == rows[j].BaseURL
			case "stage_count":
				less, eq = rows[i].StageCount < rows[j].StageCount, rows[i].StageCount == rows[j].StageCount
			case "breaker_paused":
				less, eq = !rows[i].BreakerPaused && rows[j].BreakerPaused, rows[i].BreakerPaused == rows[j].BreakerPaused
			case "breaker_paused_at":
				less = rows[i].BreakerPausedAt.Before(rows[j].BreakerPausedAt)
				eq = rows[i].BreakerPausedAt.Equal(rows[j].BreakerPausedAt)
			default:
				continue
			}
			if eq {
				continue
			}
			if f.Desc {
				return !less
			}
			return less
		}
		return false
	})
}

// pageBounds 页码/每页条数算成切片区间；越界时给一个空区间（不是 panic）。
func pageBounds(page, size, n int) (int, int) {
	if size <= 0 {
		size = 50
	}
	if page <= 0 {
		page = 1
	}
	if n <= 0 {
		return 0, 0
	}
	// 用除法夹页码：page 极大时 (page-1)*size 会溢出成负数，绕过下面的比较直接切片 panic。
	// 第 page 页存在当且仅当 page-1 <= (n-1)/size（最后一页的页号），超过就是空区间。
	if page-1 > (n-1)/size {
		return n, n
	}
	from := (page - 1) * size
	if from > n {
		from = n
	}
	to := from + size
	if to > n {
		to = n
	}
	return from, to
}
