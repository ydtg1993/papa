package models

import (
	"strings"
	"testing"
)

// 表名与列名钉死。gorm 的命名策略会把 `CfgKey` 推成 `cfg_key`，但那只是推断 ——
// 后台表格页、运维手写 SQL、以及文档都按**真实列名**来，改字段名时不能悄悄把列名也改掉。
//
// 另外 `key` / `val` 是刻意没用的：`KEY` 是 MySQL 保留字，这个仓库为它吃过一次 1064。
func TestConfigTableShape(t *testing.T) {
	db := dryRun(t)

	rec := Config{CfgKey: "ip_whitelist", CfgVal: []byte(`["10.0.0.0/8"]`), UpdatedBy: "alice"}
	// 读**返回的那个 tx** 上的 Statement：db.Create 走的是 getInstance 克隆，
	// 原始的 db.Statement 里没有拼出来的 SQL（与 engine/sqlshape_test.go 的 renderSQL 同一写法）。
	tx := db.Create(&rec)
	if tx.Error != nil {
		t.Fatalf("create: %v", tx.Error)
	}
	sql := tx.Statement.SQL.String()
	for _, want := range []string{"`crawler_config`", "`cfg_key`", "`cfg_val`", "`updated_by`"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("拼出来的 SQL 里缺 %q：\n%s", want, sql)
		}
	}
	if got := (Config{}).TableName(); got != "crawler_config" {
		t.Fatalf("表名 = %q，与其它模型的 crawler_ 前缀不一致", got)
	}
}
