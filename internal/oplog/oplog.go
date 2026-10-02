package oplog

import (
	"encoding/json"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/internal/gormsource"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Recorder 把 oao 的操作事件写进 crawler_operation_log。
// 它只关心"记没记下来"，写失败只记日志，不影响业务操作本身。
type Recorder struct {
	db  *gorm.DB
	log interface {
		Errorf(format string, args ...any)
	}
}

// New 创建记录器。
func New(db *gorm.DB, logger interface {
	Errorf(format string, args ...any)
}) *Recorder {
	return &Recorder{db: db, log: logger}
}

// Event 一次后台写操作。有两类生产者：oao 表格页的动作（经 Record 适配），
// 以及后台自带的原生接口（访问令牌页，直接调 RecordEvent）。
type Event struct {
	Table    string
	Action   string
	RowID    string
	Values   map[string]any
	Err      error // nil 表示成功
	IP       string
	Operator string // 谁干的；未鉴权/测试构造的请求为空
	At       time.Time
}

// Record 实现 oao.Config.OnAction：成功与失败都入库。
// operator 由调用方从请求上下文里取（组件只把请求带出来，不解释身份）。
func (r *Recorder) Record(ev oao.ActionEvent, operator string) {
	r.RecordEvent(Event{
		Table: ev.Table, Action: ev.Action, RowID: ev.ID, Values: ev.Values,
		Err: ev.Err, IP: ev.IP, Operator: operator, At: ev.At,
	})
}

// RecordEvent 记一次写操作；写失败只记日志，不影响业务操作本身。
func (r *Recorder) RecordEvent(ev Event) {
	rec := newRecord(ev)
	if err := r.db.Create(&rec).Error; err != nil {
		// 审计写失败不能反过来影响业务操作，只记日志
		r.log.Errorf("write operation log: %s", err.Error())
	}
}

// newRecord 把操作事件映射成日志行（纯函数，便于单测）。
// 失败也要记：失败的删除/编辑同样是要查的线索。
func newRecord(ev Event) models.OperationLog {
	values := datatypes.JSON("{}")
	if len(ev.Values) > 0 {
		if b, err := json.Marshal(ev.Values); err == nil {
			values = datatypes.JSON(b)
		}
	}
	rec := models.OperationLog{
		Table:     ev.Table,
		Action:    ev.Action,
		RowID:     ev.RowID,
		Values:    values,
		Operator:  ev.Operator,
		OK:        ev.Err == nil,
		IP:        ev.IP,
		CreatedAt: ev.At,
	}
	if ev.Err != nil {
		rec.Error = ev.Err.Error()
	}
	return rec
}

// Table 把操作日志本身作为一张只读表格页，方便在后台直接查。
func Table(db *gorm.DB) oao.Table {
	return oao.Table{
		Key: "operation_log", Label: "操作日志", Group: "数据",
		Source: gormsource.New(gormsource.Config{
			DB:     db,
			Model:  &models.OperationLog{},
			Search: []string{"table", "action", "row_id", "operator", "error", "ip"},
		}),
		Columns: []oao.Column{
			{Field: "id", Kind: oao.KindNumber, Width: "70px"},
			{Field: "created_at", Label: "时间", Kind: oao.KindTime, Width: "170px"},
			{Field: "table", Label: "表格"},
			{Field: "action", Label: "动作"},
			{Field: "row_id", Label: "行 ID", Width: "90px"},
			{Field: "operator", Label: "操作人", Width: "110px"},
			{Field: "ok", Label: "结果", Kind: oao.KindBool, Render: oao.RenderEnum,
				Enum: map[string]string{"true": "成功", "false": "失败"},
				Tone: map[string]string{"true": "ok", "false": "err"}},
			{Field: "values", Label: "提交值", Kind: oao.KindJSON},
			{Field: "error", Label: "失败原因", Render: oao.RenderInput, MaxLen: 40},
			{Field: "ip", Label: "来源 IP", Width: "130px"},
		},
		Filters: []oao.Filter{
			{Field: "table", Label: "表格"},
			{Field: "action", Label: "动作"},
			{Field: "operator", Label: "操作人", Op: oao.OpLike},
			{Field: "ok", Label: "结果", Kind: oao.KindBool, Op: oao.OpIn,
				Options: map[string]string{"true": "成功", "false": "失败"}},
			{Field: "created_at", Label: "时间", Kind: oao.KindTime, Op: oao.OpBetween},
		},
		DefaultSort: "-id",
		PageSize:    20,
	}
}
