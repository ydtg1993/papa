package models

import (
	"testing"

	"gorm.io/gorm"
)

// crawler_tasks 的索引声明（schema.ParseIndexes 是 gorm 迁移真正读的那份，别只信 tag 文本）：
//   - idx_repeat_due 是 (repeatable, status, next_repeat_at) 的复合索引（轮询判据）；
//   - idx_task_status 是 status 单列（恢复队列 / 去重预热按 status 过滤）。
//
// 同一个字段挂两个 `index:` tag 在 gorm 里是允许的，但很容易被 TagSettings 只看最后一个值的
// 直觉写错；这条用 ParseIndexes 把两条都钉住 —— 撤掉任何一个 tag 都会红。
func TestCrawlerTaskIndexes(t *testing.T) {
	stmt := &gorm.Statement{DB: dryRun(t)}
	if err := stmt.Parse(&CrawlerTask{}); err != nil {
		t.Fatalf("parse model: %v", err)
	}

	indexes := map[string][]string{} // name -> 字段顺序
	unique := map[string]bool{}
	for _, idx := range stmt.Schema.ParseIndexes() {
		fields := make([]string, 0, len(idx.Fields))
		for _, f := range idx.Fields {
			fields = append(fields, f.Field.DBName)
		}
		indexes[idx.Name] = fields
		unique[idx.Name] = idx.Class == "UNIQUE"
	}

	repeatDue, ok := indexes["idx_repeat_due"]
	if !ok {
		t.Fatal("缺 idx_repeat_due（轮询判据复合索引）")
	}
	wantDue := []string{"repeatable", "status", "next_repeat_at"}
	if len(repeatDue) != len(wantDue) {
		t.Fatalf("idx_repeat_due 字段 = %v, want %v", repeatDue, wantDue)
	}
	for i := range wantDue {
		if repeatDue[i] != wantDue[i] {
			t.Fatalf("idx_repeat_due 字段 = %v, want %v（顺序是判据，不能乱）", repeatDue, wantDue)
		}
	}

	status, ok := indexes["idx_task_status"]
	if !ok {
		t.Fatal("缺 idx_task_status（status 单列索引）")
	}
	if len(status) != 1 || status[0] != "status" {
		t.Fatalf("idx_task_status 字段 = %v, want [status]", status)
	}
	if unique["idx_task_status"] {
		t.Fatal("idx_task_status 不该是唯一索引")
	}

	if !unique["idx_site_stage_url_hash"] {
		t.Fatal("idx_site_stage_url_hash 必须仍是唯一索引")
	}
}
