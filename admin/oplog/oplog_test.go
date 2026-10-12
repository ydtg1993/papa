package oplog

import (
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/oao"
)

// New 必须拦住"不可用的 logger"，而且是**当场**拦下。
//
// 回归点：以前不判，隐患是**延迟**的 —— 传进去的 logger 一直是摆设，直到消费协程里某次
// 写库失败或队列满，才在 `fallback` 里 nil 解引用把整个进程带走：启动一声不响，
// 跑着跑着突然崩，现场还离"审计"十万八千里。
//
// db 传 nil 是有意的：判 logger 发生在碰 db 之前，这条例外不该走到后面去。
func TestNewPanicsOnUnusableLogger(t *testing.T) {
	t.Run("未类型化的 nil", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("传 nil logger 应当当场 panic")
			}
		}()
		New(nil, nil)
	})

	// 最常见的误用形态：宿主把自己的一个**没初始化**的 *logrus.Logger 字段传进来
	// （比如手搓的 LoggerSet 少填了 Sys）。赋给接口后接口值本身不为 nil（`logger == nil`
	// 是 false），但调用它的方法照样 nil 解引用 —— 只判 `== nil` 会漏掉这一种。
	t.Run("类型化的 nil（接口值本身不为 nil）", func(t *testing.T) {
		var l *memLogger
		defer func() {
			if recover() == nil {
				t.Fatal("传类型化 nil 的 logger 也应当当场 panic")
			}
		}()
		New(nil, l)
	})
}

func TestNewRecordSuccess(t *testing.T) {
	at := time.Now()
	rec := newRecord(Event{
		Table: "order", Action: "approve", RowID: "42",
		Values: map[string]any{"reason": "ok"}, IP: "10.0.0.1", Operator: "张三", At: at,
	})

	if rec.Table != "order" || rec.Action != "approve" || rec.RowID != "42" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Operator != "张三" {
		t.Fatalf("操作人没入库：%+v", rec)
	}
	if !rec.OK || rec.Error != "" {
		t.Fatalf("成功事件应 OK=true 且无错误：%+v", rec)
	}
	if rec.IP != "10.0.0.1" || !rec.CreatedAt.Equal(at) {
		t.Fatalf("IP/时间没带上：%+v", rec)
	}
	if string(rec.Values) != `{"reason":"ok"}` {
		t.Fatalf("values = %s", rec.Values)
	}
	if rec.TableName() != "crawler_operation_log" {
		t.Fatalf("表名 = %s", rec.TableName())
	}
}

// 失败的删除/编辑同样是要查的线索，必须入库。
func TestNewRecordFailure(t *testing.T) {
	rec := newRecord(Event{
		Table: "order", Action: "remove", RowID: "7",
		Err: oao.Fail(409, "该行已被他人修改"), Operator: "张三", At: time.Now(),
	})
	if rec.OK {
		t.Fatal("失败事件应 OK=false")
	}
	if rec.Error != "该行已被他人修改" {
		t.Fatalf("error = %q", rec.Error)
	}
}

func TestNewRecordPlainError(t *testing.T) {
	rec := newRecord(Event{Table: "t", Action: "a", Err: errors.New("boom"), Operator: "张三"})
	if rec.OK || rec.Error != "boom" {
		t.Fatalf("record = %+v", rec)
	}
}

// 没有表单值的操作（确认型）也要能落库，values 为空对象而不是 NULL。
func TestNewRecordNoValues(t *testing.T) {
	rec := newRecord(Event{Table: "t", Action: "approve", Operator: "张三"})
	if string(rec.Values) != "{}" {
		t.Fatalf("values = %q, want {}", rec.Values)
	}
}

// 表格声明要能被组件校验通过（顺手挡住"列重复"这类手误），并带上「操作人」列与筛选。
func TestTableDeclaration(t *testing.T) {
	o, err := oao.New(oao.Config{Tables: []oao.Table{Table(nil)}})
	if err != nil {
		t.Fatalf("oao.New 拒绝了声明: %v", err)
	}
	info, ok := o.Table("operation_log")
	if !ok {
		t.Fatal("表 key 应为 operation_log")
	}
	var hasCol, hasFilter bool
	for _, c := range info.Columns {
		if c.Name == "operator" {
			hasCol = true
		}
	}
	for _, f := range info.Filters {
		if f.Name == "operator" {
			hasFilter = true
		}
	}
	if !hasCol || !hasFilter {
		t.Fatalf("operator 列=%v 筛选=%v，两个都该有", hasCol, hasFilter)
	}
}
