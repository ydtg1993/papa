package database

import "gorm.io/gorm"

// dropLegacyTaskColumns 删掉 crawler_tasks 上的 title / content 两列。
//
// 为什么必须显式删：AutoMigrate **只增不减** —— 模型里把字段去掉，它什么都不做，两列会永远留在库里；
// 而且后台任务表的搜索声明里一旦还带着 title，真库上就是 Error 1054（DryRun 那种只拼 SQL 的测试看不见）。
// 这两个列曾经是"任务结果"的落点，现在结果由业务自己建表落（框架不替业务决定表结构）。
//
// 与 migrate_taskurl.go 里的其它显式迁移同一套路：先查再改、可重复跑、**表不存在就整段跳过**
// （全新库交给后面的 AutoMigrate 按新结构建表）。
func dropLegacyTaskColumns(db *gorm.DB) error {
	const table = "crawler_tasks"
	exists, err := tableExists(db, table)
	if err != nil || !exists {
		return err
	}
	for _, col := range []string{"title", "content"} {
		if err := dropColumnIfExists(db, table, col); err != nil {
			return err
		}
	}
	return nil
}
