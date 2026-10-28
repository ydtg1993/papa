package app

import (
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/database"
	"gorm.io/gorm"
)

// frameworkIndexHints 是启动时要确认存在的框架索引，以及"缺了会怎样"的提示语。
// 这里只做**只读探测**：真正建索引在 `papa migrate`（AutoMigrate）里，启动不自动迁。
var frameworkIndexHints = []struct {
	table, index, hint string
}{
	{
		table: "crawler_tasks", index: "idx_site_stage_url_hash",
		hint: "唯一索引缺了，去重就只剩内存表那一层，重启后同一个 URL 会再插一行",
	},
	{
		table: "crawler_tasks", index: "idx_task_status",
		hint: "status 单列索引缺了，恢复队列/去重预热按 status 过滤会退化成全表扫描",
	},
}

// checkFrameworkSchema 启动期的框架表/索引检查（NewApp 里调用）。
//
// 返回缺失的框架表名、缺失的框架索引名。err 表示**查询本身**失败（这时结果不可信，
// 调用方只记日志、不影响启动 —— 与原来"表不在只喊一声"同一取向）。
//
// 之所以单独成一个函数：NewApp 里那一步要真库，这两个判据靠一条 information_schema 查询
// 拿全，拆出来就能用假库离线验"缺表/缺索引/查询失败"三种结局。
func checkFrameworkSchema(db *gorm.DB, cfg *config.Config) (missingTables []string, missingIndexes []string, err error) {
	models := database.FrameworkModels(cfg)
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}

	existing, err := database.ExistingTables(db, names)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range models {
		if !existing[m.Name] {
			missingTables = append(missingTables, m.Name)
		}
	}
	for _, check := range frameworkIndexHints {
		if !existing[check.table] {
			continue // 表都不在：已包含在缺表里，不再逐个报缺索引
		}
		has, err := database.HasIndexNamed(db, check.table, check.index)
		if err != nil {
			return missingTables, nil, err
		}
		if !has {
			missingIndexes = append(missingIndexes, check.index)
		}
	}
	return missingTables, missingIndexes, nil
}
