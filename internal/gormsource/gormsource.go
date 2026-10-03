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
				clauses = append(clauses, quote(c)+" LIKE ?")
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

// quote 给列名套反引号。列名来自表格声明而不是 HTTP，但**不加引号照样会炸**：
// 内置「操作日志」表就有一个叫 `table` 的列，而 table 是 MySQL 保留字 ——
// 裸拼进 SQL 就是语法错，整页 500（排序那条一直在加引号，筛选/搜索这两条漏了）。
func quote(col string) string { return "`" + col + "`" }

// boolList 把 OpIn 的值逐项归一成 bool（gorm 会把 []bool 渲染成 true/false 字面量，
// MySQL 拿它跟 tinyint 比是对的）。
//
// 认的写法与 oao 的 FilterValue.Bool() 保持一致；认不出的项跳过（与 IntList 对 Atoi
// 失败的宽容度一致），全认不出就当没填这个筛选。
func boolList(f oao.FilterValue) []bool {
	var out []bool
	for _, p := range f.List() {
		switch strings.ToLower(p) {
		case "1", "true", "yes":
			out = append(out, true)
		case "0", "false", "no":
			out = append(out, false)
		}
	}
	return out
}

// applyFilter 按声明里的算子拼一条条件。字段名来自表格声明（不是 HTTP），可以安全拼进 SQL。
func applyFilter(db *gorm.DB, f oao.FilterValue) *gorm.DB {
	col := quote(f.Field())
	switch f.Op() {
	case oao.OpLike:
		return db.Where(col+" LIKE ?", "%"+f.Raw()+"%")

	case oao.OpPrefix:
		// 前缀匹配：只有后通配，能走索引（OpLike 前后都通配，用不上索引）
		return db.Where(col+" LIKE ?", f.String()+"%")

	case oao.OpIn:
		if f.Kind() == oao.KindNumber {
			if vals := f.IntList(); len(vals) > 0 {
				return db.Where(col+" IN ?", vals)
			}
			return db
		}
		if f.Kind() == oao.KindBool {
			// bool 列在 MySQL 里是 tinyint，把 "true" 直接塞进 IN 会被转成 0 ——
			// 跟拿 'abc' 跟数值比是同一个道理，于是「成功」筛出一堆失败行。
			if vals := boolList(f); len(vals) > 0 {
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
