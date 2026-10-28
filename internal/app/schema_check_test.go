package app

import (
	"database/sql/driver"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// 启动期结构检查的三条结局：缺表 / 缺索引 / 查询失败。
// 用假库（能返回行、能按语句配错误）—— 真库那条路只在 NewApp 里跑一次，这里验判据本身。
func TestCheckFrameworkSchema(t *testing.T) {
	zeroCfg := &config.Config{} // 操作日志/追踪都关：框架表只剩 tasks / access_token / config

	t.Run("三张表都在、两个索引导都在", func(t *testing.T) {
		f := gormfake.New()
		f.SetRow(map[string]driver.Value{"table_name": "crawler_tasks"})
		f.SetCounts(1, 1) // 两个 HasIndexNamed 的 COUNT 都走 countAll

		// 假库只回一行：只证明 crawler_tasks 在；另外两张按"缺失"返回。
		missing, missingIndexes, err := checkFrameworkSchema(gormfake.MustOpen(f), zeroCfg)
		if err != nil {
			t.Fatalf("checkFrameworkSchema = %v", err)
		}
		if len(missingIndexes) != 0 {
			t.Fatalf("索引都在时不该报缺索引，实得 %v", missingIndexes)
		}
		want := []string{"crawler_access_token", "crawler_config"}
		if len(missing) != len(want) {
			t.Fatalf("missing = %v, want %v（假库只回了一行）", missing, want)
		}
		for i := range want {
			if missing[i] != want[i] {
				t.Fatalf("missing = %v, want %v", missing, want)
			}
		}
	})

	t.Run("表都不在 → 全部报缺，且不逐个查索引", func(t *testing.T) {
		f := gormfake.New()
		f.SetRow(nil)

		missing, missingIndexes, err := checkFrameworkSchema(gormfake.MustOpen(f), zeroCfg)
		if err != nil {
			t.Fatalf("checkFrameworkSchema = %v", err)
		}
		if len(missingIndexes) != 0 {
			t.Fatalf("表都不在时不该再报缺索引（已包含在缺表里），实得 %v", missingIndexes)
		}
		if len(missing) != 3 {
			t.Fatalf("missing = %v, want 3 张框架表", missing)
		}
	})

	t.Run("crawler_tasks 在、索引导都不在 → 两个索引都报", func(t *testing.T) {
		f := gormfake.New()
		f.SetRow(map[string]driver.Value{"table_name": "crawler_tasks"})
		f.SetCounts(0, 0)

		_, missingIndexes, err := checkFrameworkSchema(gormfake.MustOpen(f), zeroCfg)
		if err != nil {
			t.Fatalf("checkFrameworkSchema = %v", err)
		}
		// 假库对两条 COUNT 返回同一个值，所以这里只能验"两个都缺"；真库上二者独立。
		want := map[string]bool{"idx_site_stage_url_hash": true, "idx_task_status": true}
		for _, idx := range missingIndexes {
			delete(want, idx)
		}
		if len(missingIndexes) != 2 || len(want) != 0 {
			t.Fatalf("missingIndexes = %v, want 两个都缺", missingIndexes)
		}
	})

	t.Run("查询失败 → 返回错误（调用方只记日志，不阻止启动）", func(t *testing.T) {
		f := gormfake.New()
		f.SetQueryError("information_schema", "db down")

		if _, _, err := checkFrameworkSchema(gormfake.MustOpen(f), zeroCfg); err == nil {
			t.Fatal("查询失败应当返回错误")
		}
	})
}

// 启动检查要确认的索引名必须和模型/迁移里的一致：写错字等于永远报"缺索引"或永远查不到。
func TestFrameworkIndexHintsMatchModelIndexes(t *testing.T) {
	want := map[string]bool{
		"idx_site_stage_url_hash": true, // 去重唯一索引
		"idx_task_status":         true, // status 单列（恢复队列/去重预热）
	}
	for _, c := range frameworkIndexHints {
		if c.table != "crawler_tasks" {
			t.Fatalf("索引检查表 = %q, want crawler_tasks", c.table)
		}
		if !want[c.index] {
			t.Fatalf("多出/拼错的索引检查项 %q", c.index)
		}
		delete(want, c.index)
	}
	if len(want) != 0 {
		t.Fatalf("缺少索引检查项 %v", want)
	}
}
