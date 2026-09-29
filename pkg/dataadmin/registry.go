package dataadmin

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// ErrUnknownModel 表示查询了未登记的模型
var ErrUnknownModel = errors.New("dataadmin: unknown model")

// Kind 列的值类型，用于前端渲染与筛选值强转
type Kind string

const (
	KindString Kind = "string"
	KindNumber Kind = "number"
	KindBool   Kind = "bool"
	KindTime   Kind = "time"
	KindJSON   Kind = "json"
)

// Column 一列的元数据
type Column struct {
	Name       string `json:"name"`       // 数据库列名(snake_case)
	Label      string `json:"label"`      // 展示名
	Kind       Kind   `json:"kind"`       // 值类型
	Searchable bool   `json:"searchable"` // 参与模糊搜索
	Sortable   bool   `json:"sortable"`   // 可排序
	Filterable bool   `json:"filterable"` // 可等值筛选
}

// ModelInfo 一个可浏览模型
type ModelInfo struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Table   string   `json:"table"`
	Model   any      `json:"-"`
	Columns []Column `json:"columns"`
}

// Registry 可浏览模型的注册表，负责内省列与执行查询
type Registry struct {
	db     *gorm.DB
	mu     sync.RWMutex
	models map[string]*ModelInfo
}

// New 创建注册表
func New(db *gorm.DB) *Registry {
	return &Registry{db: db, models: make(map[string]*ModelInfo)}
}

// Register 登记一个模型：key 为 URL 安全标识，label 为展示名，model 为结构体指针
func (r *Registry) Register(key, label string, model any) error {
	s, err := schema.Parse(model, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return fmt.Errorf("parse model %T: %w", model, err)
	}
	if label == "" {
		label = key
	}
	info := &ModelInfo{Key: key, Label: label, Table: s.Table, Model: model}
	for _, f := range s.Fields {
		if f.IgnoreMigration {
			continue
		}
		kind := kindOf(f.FieldType)
		info.Columns = append(info.Columns, Column{
			Name:       f.DBName,
			Label:      humanize(f.DBName),
			Kind:       kind,
			Searchable: kind == KindString,
			Sortable:   kind == KindString || kind == KindNumber || kind == KindBool || kind == KindTime,
			Filterable: kind == KindString || kind == KindNumber || kind == KindBool || kind == KindTime,
		})
	}

	r.mu.Lock()
	r.models[key] = info
	r.mu.Unlock()
	return nil
}

// Models 返回全部已登记模型（按 key 排序）
func (r *Registry) Models() []*ModelInfo {
	r.mu.RLock()
	out := make([]*ModelInfo, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Get 按 key 取模型
func (r *Registry) Get(key string) (*ModelInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[key]
	return m, ok
}

var timeType = reflect.TypeOf(time.Time{})

// kindOf 把 Go 字段类型映射为 Kind；指针解引用后判断
func kindOf(ft reflect.Type) Kind {
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	if ft == timeType {
		return KindTime
	}
	switch ft.Kind() {
	case reflect.String:
		return KindString
	case reflect.Bool:
		return KindBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return KindNumber
	default:
		return KindJSON
	}
}

// humanize 把 snake_case 列名转为展示名："created_at" -> "Created At"
func humanize(s string) string {
	words := strings.Split(s, "_")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
