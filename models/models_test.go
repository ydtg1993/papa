package models

import (
	"testing"

	"gorm.io/datatypes"
)

// content 列是 JSON 且要参与业务解析：业务没写内容时必须落 {}，不能落 NULL / 空串。
func TestCrawlerTaskBeforeCreateFillsContent(t *testing.T) {
	cases := []struct {
		name string
		in   datatypes.JSON
	}{
		{"nil", nil},
		{"空串", datatypes.JSON("")},
	}
	for _, c := range cases {
		task := &CrawlerTask{Content: c.in}
		if err := task.BeforeCreate(nil); err != nil {
			t.Fatalf("%s: BeforeCreate = %v", c.name, err)
		}
		if string(task.Content) != "{}" {
			t.Fatalf("%s: content 应补成 {}，实得 %q", c.name, task.Content)
		}
	}

	// 已写好的内容不能被钩子覆盖
	task := &CrawlerTask{Content: datatypes.JSON(`{"a":1}`)}
	if err := task.BeforeCreate(nil); err != nil {
		t.Fatalf("BeforeCreate = %v", err)
	}
	if string(task.Content) != `{"a":1}` {
		t.Fatalf("已有内容被改写：%q", task.Content)
	}
}

// 这三张表显式声明了表名（不走 gorm 的复数化规则）；改名字等于改线上表名，钉住它。
func TestTableNames(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{AccessToken{}.TableName(), "crawler_access_token"},
		{OperationLog{}.TableName(), "crawler_operation_log"},
		{TaskTrace{}.TableName(), "crawler_task_trace"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Fatalf("表名 = %q, want %q", c.got, c.want)
		}
	}
}

// 状态值是落到库里的契约：前端枚举（admin/tasksource 的 map 用 "0".."3" 当 key）、
// SQL 条件、存量数据都按它解释。改动它会静默错位，所以钉住。
func TestStatusEnumValues(t *testing.T) {
	if TaskStatusPending != 0 || TaskStatusProcessing != 1 ||
		TaskStatusSuccess != 2 || TaskStatusFailed != 3 {
		t.Fatalf("TaskStatus 取值变了：pending=%d processing=%d success=%d failed=%d",
			TaskStatusPending, TaskStatusProcessing, TaskStatusSuccess, TaskStatusFailed)
	}
	if RepeatableNo != 0 || RepeatableYes != 1 {
		t.Fatalf("RepeatableStatus 取值变了：no=%d yes=%d", RepeatableNo, RepeatableYes)
	}
	if TraceOK != 0 || TraceFailed != 1 {
		t.Fatalf("TraceStatus 取值变了：ok=%d failed=%d", TraceOK, TraceFailed)
	}
}
