package database

import (
	"gorm.io/gorm"
)

// ExistingTables 一次查询返回 names 里在库里存在的表名集合。
//
// 为什么不用 db.Migrator().HasTable：gorm 的 MySQL 迁移器每次都会先 CurrentDatabase()
// （`SELECT DATABASE()` + `SCHEMA_NAME FROM information_schema.SCHEMATA ...`）再查
// `information_schema.tables` —— **一张表 3 条 SQL**。启动期检查十几张框架表就是几十条
// 毫无信息量的查询（运行日志也被刷屏）。这里一条 information_schema 查询就够。
//
// 只支持 MySQL：框架的 DB 层本来就只接受 mysql（见 NewDB），不做别的方言的通用实现。
func ExistingTables(db *gorm.DB, names []string) (map[string]bool, error) {
	out := make(map[string]bool, len(names))
	if db == nil || len(names) == 0 {
		return out, nil
	}
	var rows []struct{ TableName string }
	if err := db.Raw(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.TableName] = true
	}
	return out, nil
}

// HasIndexNamed 判断某张表上有没有这个索引名（一次 information_schema.statistics 查询）。
func HasIndexNamed(db *gorm.DB, table, index string) (bool, error) {
	if db == nil || table == "" || index == "" {
		return false, nil
	}
	var n int64
	// 用 Row().Scan 而不是 Scan(&struct)：COUNT 是一列结果集，直接扫进 int64 最省事，
	// 也不依赖驱动给这一列起什么名字。
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`, table, index).
		Row().Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
