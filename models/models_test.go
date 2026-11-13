package models

import (
	"testing"
)

// 幂等键的哈希：与 MySQL 的 SHA2(x,256) 同值（迁移用 SQL 回填、运行时用 Go 算，两边必须一致）。
// 空键返回空串 —— 调用方据此落 NULL（唯一索引下多个 NULL 互不相同，空串会互相撞）。

// BeforeCreate 补哈希：有自定义键 → 补上；**没有键 → 保持 nil（落 NULL）**。

// 这三张表显式声明了表名（不走 gorm 的复数化规则）；改名字等于改线上表名，钉住它。
func TestTableNames(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{AccessToken{}.TableName(), "crawler_access_token"},
		{OperationLog{}.TableName(), "crawler_operation_log"},
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
}

// URL 的哈希是**定长**的（唯一索引用它，长 URL 建不了整串索引），算法必须与迁移里的
// `SHA2(url, 256)` 一致：sha256 的十六进制小写。

// 任何创建路径都要带上哈希（唯一索引建在它上面，空串会自己撞自己）。

func TestBeforeCreateCurrentIdentity(t *testing.T) {
	row := CrawlerTask{Site: "shop", Stage: "detail", URL: "https://EXAMPLE.test:443/a#x"}
	if err := row.BeforeCreate(nil); err != nil {
		t.Fatal(err)
	}
	if row.IdentityType != "url" || row.IdentityValue != "https://example.test/a" || len(row.IdentityHash) != 64 {
		t.Fatalf("identity: %+v", row)
	}
	row.IdentityType = "id"
	row.IdentityValue = "resource-1"
	if err := row.BeforeCreate(nil); err != nil {
		t.Fatal(err)
	}
	if row.IdentityValue != "resource-1" {
		t.Fatal("resource identity lost")
	}
}
