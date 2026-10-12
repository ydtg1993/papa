package models

import (
	"strings"
	"testing"
)

// 这三张表显式声明了表名（不走 gorm 的复数化规则）；改名字等于改线上表名，钉住它。
func TestTableNames(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{AccessToken{}.TableName(), "crawler_access_token"},
		{CrawlerSite{}.TableName(), "crawler_sites"},
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
	// TraceWarn 是**追加**在末尾的：老行里的 0/1 含义不能变（改中间那个就得洗数据）
	if TraceOK != 0 || TraceFailed != 1 || TraceWarn != 2 {
		t.Fatalf("TraceStatus 取值变了：ok=%d failed=%d warn=%d", TraceOK, TraceFailed, TraceWarn)
	}
}

// URL 的哈希是**定长**的（唯一索引用它，长 URL 建不了整串索引），算法必须与迁移里的
// `SHA2(url, 256)` 一致：sha256 的十六进制小写。
func TestUrlHashIsSHA256Hex(t *testing.T) {
	// 已知向量（同一个值 MySQL 的 SHA2('abc',256) 也是它 —— 两边能对上，回填过的老行
	// 与新建的行才不会各算各的）
	if got := UrlHash("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("UrlHash(abc) = %q", got)
	}
	// 长 URL 一样是 64 字符（这正是要的效果）
	long := "https://example.com/detail/126001/?a=" + strings.Repeat("x", 2000)
	if got := UrlHash(long); len(got) != 64 {
		t.Fatalf("长 URL 的哈希也应当是 64 字符，实得 %d", len(got))
	}
	// 不同 URL 不同哈希
	if UrlHash("https://a/") == UrlHash("https://b/") {
		t.Fatal("不同 URL 不该同哈希")
	}
}

// 任何创建路径都要带上哈希（唯一索引建在它上面，空串会自己撞自己）。
func TestBeforeCreateFillsUrlHash(t *testing.T) {
	task := &CrawlerTask{Stage: "stub", URL: "https://example.com/x"}
	if err := task.BeforeCreate(nil); err != nil {
		t.Fatal(err)
	}
	if task.URLHash != UrlHash(task.URL) {
		t.Fatalf("BeforeCreate 应当补上 URLHash，实得 %q", task.URLHash)
	}
	// 已经给了就不覆盖（迁移/回填过来的值说了算）
	task2 := &CrawlerTask{URL: "https://example.com/x", URLHash: "preset"}
	_ = task2.BeforeCreate(nil)
	if task2.URLHash != "preset" {
		t.Fatalf("不该覆盖已有的哈希，实得 %q", task2.URLHash)
	}
}
