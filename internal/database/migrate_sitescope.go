package database

import "gorm.io/gorm"

// 本文件是**一次显式迁移**：删掉 `crawler_sites` 里 `key` 为空的那一行。
//
// 那一行是"默认 scope"（未归属站点的阶段）时代的产物：`crawler_tasks.site` 是后加的列，
// 更早入库的行是空串，于是站点表里也给空串留了一行（后台把它显示成「默认 scope」）。
// 现在**站点是必填维度**：声明必须有 Key（`validateSites` 拒没有 Key 的）、
// 提交的任务必须有 site（`SubmitTask` 拒空串），所以那个 scope 不再存在 ——
// 留着这行的后果不是"少一条数据"，而是后台**一直多一个「默认 scope」Tab**：
// `seedSiteRows` 只按声明播种（不再包含空键），没人会去刷新或删除它，它就永远挂在那儿。
//
// 为什么删行而不是让前端忽略空键：那一行还带着 `base_url` / `stage_count` / 熔断暂停状态，
// 全是已死的维度；后台的站点 Tab 与「站点」表都以这张表为准，库干净了它们自然就干净了。
//
// **不动 `crawler_tasks` 里 site 为空的行**：那是业务数据，框架不替谁决定删还是补。
// 它们的下场在文档里写清了（三条治理队列会跳过后打一条 WARN，要救就自己 `UPDATE ... SET site = ...`）。
//
// 可重复跑：行不在了就什么都不做。
func deleteDefaultScopeSiteRow(db *gorm.DB) error {
	const table = "crawler_sites"
	exists, err := tableExists(db, table)
	if err != nil || !exists {
		return err // 全新库：交给 AutoMigrate 按模型建（本来也不会建出空键那一行）
	}
	// `key` 是 MySQL 保留字，裸写进 SQL 会 Error 1064 —— 引号只写在这一处（与 engine 的 siteKeyScope 同一约定）。
	return db.Exec("DELETE FROM `crawler_sites` WHERE `key` = ''").Error
}
