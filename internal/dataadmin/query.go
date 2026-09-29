package dataadmin

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ListQuery 列表查询条件
type ListQuery struct {
	Page   int               // 页码，1 起
	Size   int               // 每页条数
	Search string            // 模糊搜索（作用于 Searchable 字符串列）
	Sort   string            // "col" 升序 / "-col" 降序
	Filter map[string]string // 列名 -> 等值
}

// ListResult 查询结果
type ListResult struct {
	Total   int64            `json:"total"`
	Page    int              `json:"page"`
	Size    int              `json:"size"`
	Columns []Column         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

// List 执行分页查询；sort/filter/search 的列名均走白名单，杜绝注入
func (r *Registry) List(key string, q ListQuery) (*ListResult, error) {
	info, ok := r.Get(key)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownModel, key)
	}
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size < 1 {
		q.Size = 20
	}
	if q.Size > 200 {
		q.Size = 200
	}

	colByName := make(map[string]Column, len(info.Columns))
	for _, c := range info.Columns {
		colByName[c.Name] = c
	}

	// 共享条件构建：每次从新实例起，避免 GORM 语句污染
	apply := func(db *gorm.DB) *gorm.DB {
		for name, val := range q.Filter {
			col, ok := colByName[name]
			if !ok || !col.Filterable {
				continue
			}
			db = db.Where(name+" = ?", coerce(val, col.Kind))
		}
		if q.Search != "" {
			var clauses []string
			var args []any
			like := "%" + q.Search + "%"
			for _, c := range info.Columns {
				if c.Searchable {
					clauses = append(clauses, c.Name+" LIKE ?")
					args = append(args, like)
				}
			}
			if len(clauses) > 0 {
				db = db.Where(strings.Join(clauses, " OR "), args...)
			}
		}
		if q.Sort != "" {
			col := q.Sort
			desc := false
			if strings.HasPrefix(col, "-") {
				desc = true
				col = col[1:]
			}
			if c, ok := colByName[col]; ok && c.Sortable {
				dir := "ASC"
				if desc {
					dir = "DESC"
				}
				db = db.Order(col + " " + dir)
			}
		}
		return db
	}

	var total int64
	if err := apply(r.db.Model(info.Model)).Count(&total).Error; err != nil {
		return nil, err
	}

	rows := []map[string]any{}
	if err := apply(r.db.Model(info.Model)).
		Offset((q.Page - 1) * q.Size).
		Limit(q.Size).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	for _, row := range rows {
		for name, v := range row {
			if col, ok := colByName[name]; ok {
				row[name] = normalize(v, col.Kind)
			}
		}
	}

	return &ListResult{Total: total, Page: q.Page, Size: q.Size, Columns: info.Columns, Rows: rows}, nil
}

// coerce 按 Kind 把筛选字符串转为对应类型
func coerce(val string, kind Kind) any {
	switch kind {
	case KindNumber:
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
		return val
	case KindBool:
		switch strings.ToLower(strings.TrimSpace(val)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no", "":
			return false
		}
		return val
	default:
		return val
	}
}

// normalize 把数据库返回的原始值归一化为便于 JSON 序列化的形态
func normalize(v any, kind Kind) any {
	if v == nil {
		return nil
	}
	switch kind {
	case KindString, KindJSON:
		return asString(v)
	case KindTime:
		if t, ok := v.(time.Time); ok {
			return t.Format(time.RFC3339)
		}
		return asString(v)
	case KindNumber:
		if b, ok := v.([]byte); ok {
			s := string(b)
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return i
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
			return s
		}
		return v
	case KindBool:
		if b, ok := v.([]byte); ok {
			switch strings.ToLower(string(b)) {
			case "1", "true":
				return true
			default:
				return false
			}
		}
		if i, ok := v.(int64); ok {
			return i != 0
		}
		return v
	default:
		return asString(v)
	}
}

// asString 把常见驱动返回值转成字符串
func asString(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}
