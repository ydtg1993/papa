package database

import (
	"database/sql/driver"
	"testing"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

func TestExistingTablesFiltersByRequestedNames(t *testing.T) {
	f := gormfake.New()
	f.SetRow(map[string]driver.Value{"table_name": "crawler_tasks"})

	got, err := ExistingTables(gormfake.MustOpen(f), []string{"crawler_tasks", "crawler_config"})
	if err != nil {
		t.Fatalf("ExistingTables = %v", err)
	}
	if !got["crawler_tasks"] {
		t.Fatalf("crawler_tasks 应判定为存在，实得 %v", got)
	}
	if got["crawler_config"] {
		t.Fatalf("假库没返回 crawler_config，不该判定存在：%v", got)
	}
}

// 只回一行时以结果集为准：查询失败必须显式报出来，不能吞成"表都不在"（那会让启动日志指错方向）。
func TestExistingTablesPropagatesError(t *testing.T) {
	f := gormfake.New()
	f.SetQueryError("information_schema", "db down")
	if _, err := ExistingTables(gormfake.MustOpen(f), []string{"crawler_tasks"}); err == nil {
		t.Fatal("查询失败应当返回错误")
	}
}

func TestSchemaCheckNilGuards(t *testing.T) {
	if got, err := ExistingTables(nil, []string{"x"}); err != nil || len(got) != 0 {
		t.Fatalf("nil db ExistingTables = (%v, %v)", got, err)
	}
	if got, err := ExistingTables(gormfake.MustOpen(gormfake.New()), nil); err != nil || len(got) != 0 {
		t.Fatalf("空 names ExistingTables = (%v, %v)", got, err)
	}
	if ok, err := HasIndexNamed(nil, "t", "i"); ok || err != nil {
		t.Fatalf("nil db HasIndexNamed = (%v, %v)", ok, err)
	}
}

func TestHasIndexNamedUsesCount(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(0, 0)
	if ok, err := HasIndexNamed(gormfake.MustOpen(f), "crawler_tasks", "idx_site_stage_url_hash"); err != nil || ok {
		t.Fatalf("COUNT=0 时 HasIndexNamed = (%v, %v), want (false, nil)", ok, err)
	}

	f2 := gormfake.New()
	f2.SetCounts(0, 1) // 查询不带 enabled → 走 countAll
	if ok, err := HasIndexNamed(gormfake.MustOpen(f2), "crawler_tasks", "idx_site_stage_url_hash"); err != nil || !ok {
		t.Fatalf("COUNT=1 时 HasIndexNamed = (%v, %v), want (true, nil)", ok, err)
	}
}
