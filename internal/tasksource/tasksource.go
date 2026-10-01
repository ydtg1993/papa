// Package tasksource 用 crawler_task 表实现 oao.Source，
// 供监控后台内置的「任务」表格使用。
//
// 这也是一个真实示例：oao 组件不碰数据层，筛选/排序/分页全在这里落地。
package tasksource

import (
	"context"
	"strconv"
	"strings"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// sortableColumns 允许排序的列白名单 —— 排序参数来自 HTTP，必须过白名单。
var sortableColumns = map[string]bool{
	"id":         true,
	"stage":      true,
	"status":     true,
	"retry":      true,
	"reprocess":  true,
	"repeat":     true,
	"created_at": true,
	"updated_at": true,
}

// searchColumns 全局搜索命中的列。
var searchColumns = []string{"url", "title", "error"}

// Source crawler_task 的数据来源。
type Source struct {
	db *gorm.DB
}

// New 创建 Source。
func New(db *gorm.DB) *Source { return &Source{db: db} }

// List 实现 oao.Source：筛选 → 搜索 → 排序 → 分页。
func (s *Source) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
	query := func(db *gorm.DB) *gorm.DB {
		db = applyFilters(db, q.Filter)
		if kw := strings.TrimSpace(q.Search); kw != "" {
			like := "%" + kw + "%"
			clauses := make([]string, 0, len(searchColumns))
			args := make([]any, 0, len(searchColumns))
			for _, c := range searchColumns {
				clauses = append(clauses, c+" LIKE ?")
				args = append(args, like)
			}
			db = db.Where(strings.Join(clauses, " OR "), args...)
		}
		return db
	}

	var total int64
	if err := query(s.db.WithContext(ctx).Model(&models.CrawlerTask{})).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	db := query(s.db.WithContext(ctx).Model(&models.CrawlerTask{}))
	if col, desc, ok := orderBy(q.Sort); ok {
		dir := "ASC"
		if desc {
			dir = "DESC"
		}
		db = db.Order("`" + col + "` " + dir)
	}
	rows := []map[string]any{}
	if err := db.Offset((q.Page - 1) * q.Size).Limit(q.Size).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// applyFilters 按声明过的算子拼筛选条件；无法识别的字段或非法值一律跳过。
func applyFilters(db *gorm.DB, filter map[string]string) *gorm.DB {
	for name, val := range filter {
		if strings.TrimSpace(val) == "" {
			continue
		}
		switch name {
		case "stage":
			db = db.Where("stage = ?", val)
		case "status":
			vals := make([]int, 0, 4)
			for _, part := range strings.Split(val, ",") {
				if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
					vals = append(vals, n)
				}
			}
			if len(vals) > 0 {
				db = db.Where("status IN ?", vals)
			}
		case "url", "title":
			db = db.Where(name+" LIKE ?", "%"+val+"%")
		}
	}
	return db
}

// orderBy 校验排序参数并返回列名与是否降序；不在白名单内则忽略排序。
func orderBy(sort string) (string, bool, bool) {
	if sort == "" {
		return "", false, false
	}
	desc := strings.HasPrefix(sort, "-")
	name := strings.TrimPrefix(sort, "-")
	if !sortableColumns[name] {
		return "", false, false
	}
	return name, desc, true
}
