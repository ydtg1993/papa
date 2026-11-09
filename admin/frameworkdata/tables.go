// Package frameworkdata exposes a read-only catalog of Papa-owned tables.
// Business tables are never discovered by prefix or registered here.
package frameworkdata

import (
	"fmt"
	"html"
	"strings"
	"sync"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/gormsource"
	"github.com/ydtg1993/papa/v4/internal/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

var labels = map[string]string{
	"crawler_task_flows":    "运行调度策略",
	"crawler_tasks":         "采集任务",
	"crawler_access_token":  "访问令牌",
	"crawler_config":        "运行配置",
	"crawler_operation_log": "操作日志",
	"crawler_task_trace":    "任务追踪",
}

func group(name string) string {
	switch name {
	case "crawler_tasks", "crawler_task_trace":
		return "任务与追踪"
	case "crawler_access_token", "crawler_config", "crawler_operation_log":
		return "系统配置"
	case "crawler_task_flows":
		return "任务调度"
	default:
		return "框架数据"
	}
}

// Tables builds declarations without querying or migrating the database.
// The actual table name is also the stable oao key, separate from action pages.
func Tables(db *gorm.DB, names ...string) ([]oao.Table, error) {
	queryDB := db
	if db != nil {
		queryDB = db.Session(&gorm.Session{Logger: logger.Discard})
	}
	var tables []oao.Table
	for _, model := range database.AllFrameworkModels() {
		if len(names) > 0 {
			selected := false
			for _, name := range names {
				if name == model.Name {
					selected = true
					break
				}
			}
			if !selected {
				continue
			}
		}
		s, err := schema.Parse(model.Value, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			return nil, fmt.Errorf("framework table %s: %w", model.Name, err)
		}
		label := labels[model.Name]
		if label == "" {
			label = model.Name
		}
		t := oao.Table{Key: model.Name, Label: label, Group: group(model.Name), PageSize: 20}
		t.Header = `<div class="muted" data-papa-framework-table><strong>` + html.EscapeString(label) + `</strong> · <code>` + html.EscapeString(model.Name) + `</code> · 只读查询。敏感字段不提供；JSON 可展开，长内容有界截断。配置值请通过系统设置查看。</div>`
		var selects, search, primary []string
		jsonFields := map[string]bool{}
		for _, f := range s.Fields {
			if f.DBName == "" || !f.Readable || hidden(model.Name, f.DBName) {
				continue
			}
			col := oao.Column{Field: f.DBName, Label: f.DBName, NoEdit: true}
			if f.Comment != "" {
				col.Label = f.DBName + " · " + f.Comment
			}
			projection := "`" + f.DBName + "`"
			switch f.GORMDataType {
			case schema.Int, schema.Uint, schema.Float:
				col.Kind = oao.KindNumber
			case schema.Bool:
				col.Kind = oao.KindBool
			case schema.Time:
				col.Kind = oao.KindTime
			default:
				projection = fmt.Sprintf("LEFT(%s, %d) AS %s", projection, maxInput+1, projection)
				col.Render = oao.RenderInput
				col.MaxLen = 32
			}
			isJSON := strings.HasSuffix(f.DBName, "_json") || f.GORMDataType == "json"
			if isJSON {
				jsonFields[f.DBName] = true
				col.Kind = oao.KindJSON
				col.Render = oao.RenderJSON
				col.NoSort = true
			}
			if strings.Contains(strings.ToLower(string(f.DataType)), "text") {
				col.NoSort = true
			}
			if f.PrimaryKey {
				primary = append(primary, f.DBName)
				col.NoSort = false
			}
			t.Columns = append(t.Columns, col)
			selects = append(selects, projection)
			// Large/unstructured text is display-only: no probing hidden JSON secrets
			// through counts and no accidental full-text scans of payloads.
			if !col.NoSort && !isJSON && f.DBName != "url" && f.DBName != "source" && f.DBName != "note" {
				op := oao.OpEq
				if col.Kind == oao.KindTime {
					op = oao.OpBetween
				}
				t.Filters = append(t.Filters, oao.Filter{Field: f.DBName, Label: f.DBName, Kind: col.Kind, Op: op})
				if f.GORMDataType == schema.String {
					search = append(search, f.DBName)
				}
			}
		}
		if len(primary) == 0 {
			return nil, fmt.Errorf("framework table %s has no primary key", model.Name)
		}
		t.IDField = primary[0]
		t.DefaultSort = "-" + primary[0]
		t.Source = &source{db: db, jsonFields: jsonFields, inner: gormsource.New(gormsource.Config{DB: queryDB, Model: model.Value, Search: search, Select: selects, TieBreakers: primary})}
		tables = append(tables, t)
	}
	return tables, nil
}

func hidden(table, field string) bool {
	return sensitive(field) || (table == "crawler_config" && field == "cfg_val") || field == "idempotency_key"
}
