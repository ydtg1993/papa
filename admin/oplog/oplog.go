package oplog

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/admin/gormsource"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// oplogQueueSize 异步队列容量。写库跟不上时宁可把记录降到日志文件，也不反压业务响应。
const oplogQueueSize = 1024

// Logger 只要一个 Errorf —— 避免为一个日志接口把宿主拉进来。
// 它也是审计的"备选落地"：这条日志走 loggers 的文件输出（lumberjack）。
type Logger interface {
	Errorf(format string, args ...any)
}

// Recorder 把 oao 的操作事件**异步**写进 crawler_operation_log。
//
// 为什么要异步：审计是每次后台增删改都要写一条的，同步写意味着业务操作得等一次 INSERT，
// 库一慢就直接拖慢后台响应。
//
// 代价是不能丢，所以有两条兜底：库写失败、或队列堵了（说明写库跟不上）时，记录会
// **落到日志文件**里 —— 落成一行 JSON，日后能捞回来补录。关停时必须 Close：
// 否则退出瞬间那几条（包括"优雅退出"这条操作本身）就没了。
type Recorder struct {
	db  *gorm.DB
	log Logger

	// mu 与 workerpool 同一套路：把「查 closed + 发送」和「置位 + close」互斥，
	// 否则关停瞬间正在发送的那条会撞上 close(ch) 直接 panic。
	mu      sync.RWMutex
	ch      chan models.OperationLog
	closed  bool
	drained chan struct{}

	dropped atomic.Int64 // 累计因队列满而降到日志的条数
}

// New 创建记录器并启动消费协程。
func New(db *gorm.DB, logger Logger) *Recorder {
	r := &Recorder{
		db:      db,
		log:     logger,
		ch:      make(chan models.OperationLog, oplogQueueSize),
		drained: make(chan struct{}),
	}
	go r.consume()
	return r
}

// consume 单协程消费：库里一条条写，慢也只慢这个协程，不挡业务响应。
func (r *Recorder) consume() {
	defer close(r.drained)
	for rec := range r.ch {
		if err := r.db.Create(&rec).Error; err != nil {
			r.fallback(rec, "写库失败: "+err.Error())
		}
	}
}

// fallback 把审计记录落到日志文件。
// 这是"备选落地"：库写不进去（或队列堵了）时，审计至少还有一份可追的副本；
// 落成 JSON 是为了日后能捞回来补录。
func (r *Recorder) fallback(rec models.OperationLog, cause string) {
	b, err := json.Marshal(rec)
	if err != nil {
		r.log.Errorf("审计记录丢失（连序列化都失败了：%s）：%+v", err.Error(), rec)
		return
	}
	r.log.Errorf("审计记录未入库（%s），已落到日志文件：%s", cause, b)
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

// RecordEvent 记一次写操作。**不阻塞**：入队即可返回，落库由消费协程做；
// 已关停或队列满时直接落到日志文件，绝不让审计反过来卡住业务操作。
func (r *Recorder) RecordEvent(ev Event) {
	rec := newRecord(ev)

	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		r.fallback(rec, "记录器已停止")
		return
	}
	select {
	case r.ch <- rec:
	default:
		r.fallback(rec, fmt.Sprintf("队列已满（容量 %d，已累计降到日志 %d 条），写库跟不上",
			cap(r.ch), r.dropped.Add(1)))
	}
}

// Close 停止接收新记录，把队列里剩下的写完（最多等 timeout）。
// 必须在关数据库之前调，否则排空时写不进去。
func (r *Recorder) Close(timeout time.Duration) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.ch)
	r.mu.Unlock()

	select {
	case <-r.drained:
	case <-time.After(timeout):
		r.log.Errorf("审计队列排空超时，还有 %d 条未落库", len(r.ch))
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
