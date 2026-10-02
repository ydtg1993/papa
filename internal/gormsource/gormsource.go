// Package gormsource 是一个基于声明驱动的 oao.Source 实现：
// 直接拿表格声明里的筛选规则拼 SQL，调用方不用再维护一份字段白名单。
//
// 它属于"业务层"—— oao 组件本身不碰数据，这里负责把声明变成 SQL。
package gormsource

import (
	"context"
	"strings"

	"github.com/ydtg1993/oao"
	"gorm.io/gorm"
)

// Config 一张表的数据源配置。
//
// 筛选不用配 —— 表格里声明了哪些 Filter、什么算子，就按什么拼；
// 列名全部来自代码里的声明，HTTP 传来的参数只能命中这些列，杜绝注入。
type Config struct {
	DB    *gorm.DB
	Model any

	// Search 全局搜索命中的列（OR LIKE）。oao 不声明"哪些字段可搜"，
	// 所以这一项仍由业务给。
	Search []string
}

// Source 声明驱动的数据源。
type Source struct {
	cfg Config
}

// New 创建数据源。
func New(cfg Config) *Source { return &Source{cfg: cfg} }

// List 实现 oao.Source。
func (s *Source) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
	apply := func(db *gorm.DB) *gorm.DB {
		for _, f := range q.Filters() {
			db = applyFilter(db, f)
		}
		if kw := strings.TrimSpace(q.Search); kw != "" && len(s.cfg.Search) > 0 {
			like := "%" + kw + "%"
			clauses := make([]string, 0, len(s.cfg.Search))
			args := make([]any, 0, len(s.cfg.Search))
			for _, c := range s.cfg.Search {
				clauses = append(clauses, c+" LIKE ?")
				args = append(args, like)
			}
			db = db.Where(strings.Join(clauses, " OR "), args...)
		}
		return db
	}

	var total int64
	if err := apply(s.cfg.DB.WithContext(ctx).Model(s.cfg.Model)).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	db := apply(s.cfg.DB.WithContext(ctx).Model(s.cfg.Model))
	// 排序字段与顺序都来自声明 + 前端参数，已过白名单；支持多字段（按优先级）
	for _, sf := range q.SortFields() {
		dir := "ASC"
		if sf.Desc {
			dir = "DESC"
		}
		db = db.Order("`" + sf.Field + "` " + dir)
	}
	rows := []map[string]any{}
	if err := db.Offset((q.Page - 1) * q.Size).Limit(q.Size).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// applyFilter 按声明里的算子拼一条条件。字段名来自表格声明（不是 HTTP），可以安全拼进 SQL。
func applyFilter(db *gorm.DB, f oao.FilterValue) *gorm.DB {
	col := f.Field()
	switch f.Op() {
	case oao.OpLike:
		return db.Where(col+" LIKE ?", "%"+f.Raw()+"%")

	case oao.OpIn:
		if f.Kind() == oao.KindNumber {
			if vals := f.IntList(); len(vals) > 0 {
				return db.Where(col+" IN ?", vals)
			}
			return db
		}
		if vals := f.List(); len(vals) > 0 {
			return db.Where(col+" IN ?", vals)
		}
		return db

	case oao.OpBetween:
		if f.Kind() == oao.KindTime {
			// 日期控件给的是 2006-01-02，用半开区间才不会漏掉结束日当天
			from, end, ok := f.DateRange()
			if !ok {
				return db
			}
			return db.Where(col+" >= ? AND "+col+" < ?", from, end)
		}
		lo, hi, ok := f.Range()
		if !ok {
			return db
		}
		return db.Where(col+" BETWEEN ? AND ?", lo, hi)

	case oao.OpGt:
		return db.Where(col+" > ?", f.Raw())

	case oao.OpLt:
		return db.Where(col+" < ?", f.Raw())

	default: // OpEq
		if f.Kind() == oao.KindNumber {
			if n, ok := f.Int(); ok {
				return db.Where(col+" = ?", n)
			}
			return db
		}
		if f.Kind() == oao.KindBool {
			if b, ok := f.Bool(); ok {
				return db.Where(col+" = ?", b)
			}
			return db
		}
		return db.Where(col+" = ?", f.Raw())
	}
}
