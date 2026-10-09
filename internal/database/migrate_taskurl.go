package database

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// 本文件是**一次显式迁移**：把 crawler_tasks 的唯一索引从 (url, stage) 换到 (url_hash, stage)。
//
// 为什么不能交给 AutoMigrate：它**只增不减**（见 Migrate 的注释），而这次要做四件事，且顺序不能乱 ——
//
//	① 删掉旧唯一索引 idx_stage_url（它建在 url 上，而 url 要改成 text：
//	   MySQL 不允许 TEXT 列不带前缀长度进索引）
//	② url 从 varchar(500) 改成 text（长 URL 不再入库失败/被截断）
//	③ 加 url_hash 列并**回填**（SHA2(url,256)，与 Go 侧 models.UrlHash 同一个值）
//	④ 建新的唯一索引 idx_stage_url_hash (url_hash, stage)
//
// ③ 必须在 ④ 之前：先建唯一索引的话，一堆还没回填的空串会自己撞自己。
//
// 每一步都先查 information_schema 判断要不要做，所以**可重复跑**；表还不存在（全新库）时整段跳过，
// 交给 AutoMigrate 按新结构建表。
func migrateTaskURLHash(db *gorm.DB) error {
	const table = "crawler_tasks"

	exists, err := tableExists(db, table)
	if err != nil || !exists {
		return err
	}

	if err := dropIndexIfExists(db, table, "idx_stage_url"); err != nil {
		return err
	}
	if err := ensureURLHashColumn(db, table); err != nil {
		return err
	}
	if err := ensureURLIsText(db, table); err != nil {
		return err
	}
	if err := backfillURLHash(db, table); err != nil {
		return err
	}
	return createUniqueIndexIfMissing(db, table, "idx_stage_url_hash", "url_hash, stage")
}

func tableExists(db *gorm.DB, table string) (bool, error) {
	var n int64
	err := db.Raw(`SELECT COUNT(*) FROM information_schema.tables
	               WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n).Error
	return n > 0, err
}

func columnExists(db *gorm.DB, table, column string) (bool, error) {
	var n int64
	err := db.Raw(`SELECT COUNT(*) FROM information_schema.columns
	               WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, column).Scan(&n).Error
	return n > 0, err
}

func columnDataType(db *gorm.DB, table, column string) (string, error) {
	var t string
	err := db.Raw(`SELECT DATA_TYPE FROM information_schema.columns
	               WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, column).Scan(&t).Error
	return t, err
}

func indexExists(db *gorm.DB, table, index string) (bool, error) {
	var n int64
	err := db.Raw(`SELECT COUNT(*) FROM information_schema.statistics
	               WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
		table, index).Scan(&n).Error
	return n > 0, err
}

func dropIndexIfExists(db *gorm.DB, table, index string) error {
	ok, err := indexExists(db, table, index)
	if err != nil || !ok {
		return err
	}
	return db.Exec(fmt.Sprintf("ALTER TABLE `%s` DROP INDEX `%s`", table, index)).Error
}

// ensureURLHashColumn 加列（先可空：表里已有行，NOT NULL 且无默认值在严格模式下加不进去）。
// 回填之后再收紧成 NOT NULL，见 backfillURLHash 之后的 step。
func ensureURLHashColumn(db *gorm.DB, table string) error {
	ok, err := columnExists(db, table, "url_hash")
	if err != nil || ok {
		return err
	}
	if err := db.Exec(fmt.Sprintf(
		"ALTER TABLE `%s` ADD COLUMN `url_hash` char(64) NULL COMMENT 'URL的sha256(长URL建不了整串唯一索引)'", table)).Error; err != nil {
		return err
	}
	return nil
}

// ensureURLIsText 把 url 改成 text（长 URL 才存得下；唯一索引已经先删掉了）。
func ensureURLIsText(db *gorm.DB, table string) error {
	dt, err := columnDataType(db, table, "url")
	if err != nil || dt == "text" || dt == "longtext" || dt == "mediumtext" {
		return err
	}
	return db.Exec(fmt.Sprintf("ALTER TABLE `%s` MODIFY `url` text NOT NULL COMMENT '任务URL'", table)).Error
}

// backfillURLHash 回填老行的哈希，并把列收紧成 NOT NULL。
//
// 哈希算法与 Go 侧（models.UrlHash）必须一致：都是 sha256 的十六进制小写 —— MySQL 的 SHA2(x,256)
// 正好就是这个形式（小写、无前缀），所以两边算出来是同一个值，迁移回填过的行与新建的行能对上。
//
// **两步都带守卫**：已经迁移过的库上一条语句都不发。不然每次 `papa migrate` 都要来一遍
// 全表 UPDATE + 一次 MODIFY —— 后者在大表上等于重建表，代价很大（而 migrate 是每次升级都会跑的）。
func backfillURLHash(db *gorm.DB, table string) error {
	missing, err := rowsMissingURLHash(db, table)
	if err != nil {
		return err
	}
	if missing > 0 {
		if err := db.Exec(fmt.Sprintf(
			"UPDATE `%s` SET `url_hash` = SHA2(`url`, 256) WHERE `url_hash` IS NULL OR `url_hash` = ''", table)).Error; err != nil {
			return err
		}
	}
	nullable, err := isNullable(db, table, "url_hash")
	if err != nil {
		return err
	}
	if !nullable {
		return nil // 已经是 NOT NULL 了，别再去 MODIFY（那会重建表）
	}
	return db.Exec(fmt.Sprintf(
		"ALTER TABLE `%s` MODIFY `url_hash` char(64) NOT NULL COMMENT 'URL的sha256(长URL建不了整串唯一索引)'", table)).Error
}

func rowsMissingURLHash(db *gorm.DB, table string) (int64, error) {
	var n int64
	err := db.Raw(fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE `url_hash` IS NULL OR `url_hash` = ''", table)).Scan(&n).Error
	return n, err
}

func isNullable(db *gorm.DB, table, column string) (bool, error) {
	var v string
	err := db.Raw(`SELECT IS_NULLABLE FROM information_schema.columns
	               WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table, column).Scan(&v).Error
	return strings.EqualFold(v, "YES"), err
}

func createUniqueIndexIfMissing(db *gorm.DB, table, index, columns string) error {
	ok, err := indexExists(db, table, index)
	if err != nil || ok {
		return err
	}
	return db.Exec(fmt.Sprintf("CREATE UNIQUE INDEX `%s` ON `%s` (%s)", index, table, columns)).Error
}
