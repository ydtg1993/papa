package oplog

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// Recorder.Record 是 oao 表格动作 → 审计那一层的适配器（app 把它接在 oao.Config.OnAction 上）：
// 每一个后台表格动作（重投/加急/删除…）都从这里过。以前它 0% 覆盖 —— 适配器写错（比如把
// 操作人丢了、把 Err 丢了）不会有任何测试红，只会表现为"操作日志里少一列"。
func TestRecorderRecordAdaptsOaoEvent(t *testing.T) {
	f := gormfake.New()
	rec := New(gormfake.MustOpen(f), &memLogger{})

	rec.Record(oao.ActionEvent{
		Table:  "task",
		Action: "retry",
		ID:     "42",
		Values: map[string]any{"reason": "手抖"},
		Err:    nil,
		IP:     "10.0.0.9",
		At:     time.Now(),
	}, "alice")
	rec.Record(oao.ActionEvent{Table: "task", Action: "fail", ID: "43", IP: "10.0.0.9", At: time.Now()},
		"bob")
	rec.Close(2 * time.Second)

	written := f.Written()
	for _, want := range []string{"crawler_operation_log", "task", "retry", "42", "alice", "10.0.0.9", "fail", "bob"} {
		if !strings.Contains(written, want) {
			t.Errorf("审计语句里缺少 %q：\n%s", want, written)
		}
	}
}
