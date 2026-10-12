package database

import (
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// 幂等键硬化（v3.4.0）：键**原文**加宽到 varchar(1000)，另加一列 `idempotency_hash` 并建唯一索引。
//
// 为什么不能直接给键原文建唯一索引：varchar(1000) 在 utf8mb4 下是 4000 字节，超过 InnoDB 的
// 3072 字节键长上限（真库实测 `ERROR 1071: Specified key was too long`）；而键里现在带 URL，
// 500 也迟早不够。于是照 `URL` / `url_hash` 那一套：**原文不建索引，唯一性由 sha256 上的唯一索引兜底**。
//
// 步骤顺序不能乱（每一步都先查 information_schema，可重复跑；表不存在就整段跳过交给 AutoMigrate）：
//
//	① 删键原文上的旧普通索引 —— 不删，加宽到 1000 就会被 1071 拦下
//	② 加宽键原文到 varchar(1000)
//	③ 加哈希列（**可空**：没自定义键的行必须落 NULL，唯一索引才放得下多行）
//	④ 查重复的非空键 —— 必须在回填**之前**：回填是把重复写进哈希列，一旦唯一索引已存在
//	   （重跑、或上一轮迁移到一半失败）那条 UPDATE 自己就会 1062，错误就变成一句看不懂的
//	   "Duplicate entry"；查重只看键原文，放前面则永远报的是人话
//	⑤ 回填哈希（与 Go 侧 models.IdempotencyKeyHash 同一个值：sha256 小写十六进制）
//	⑥ 建唯一索引
func migrateIdempotencyKey(db *gorm.DB) error {
	const table = "crawler_tasks"
	exists, err := tableExists(db, table)
	if err != nil || !exists {
		return err // 全新库：交给 AutoMigrate 按模型建（含唯一索引）
	}
	if err := dropIndexIfExists(db, table, legacyIdempotencyKeyIndex); err != nil {
		return err
	}
	if err := widenIdempotencyKey(db, table); err != nil {
		return err
	}
	if err := ensureIdempotencyHashColumn(db, table); err != nil {
		return err
	}
	if err := assertNoDuplicateIdempotencyKeys(db, table); err != nil {
		return err
	}
	if err := backfillIdempotencyHash(db, table); err != nil {
		return err
	}
	return createUniqueIndexIfMissing(db, table, idempotencyHashIndex, "idempotency_hash")
}

const (
	// legacyIdempotencyKeyIndex 是 `IdempotencyKey` 带 `index` 标签时 gorm 自动起的名（老库上有）。
	legacyIdempotencyKeyIndex = "idx_crawler_tasks_idempotency_key"
	// idempotencyHashIndex 与模型里 `uniqueIndex:` 标签写的名字必须一致，否则 AutoMigrate
	// 会以为"这个索引不存在"、在新库上再建一个同列不同名的。
	idempotencyHashIndex = "idx_idempotency_hash"
	// idempotencyKeyWidth 键原文宽度：容得下 `site:stage:<URL>`。
	idempotencyKeyWidth = 1000
)

// widenIdempotencyKey 把键原文加宽到 varchar(1000)（已够宽就不动，避免整表重建）。
func widenIdempotencyKey(db *gorm.DB, table string) error {
	var width int64
	if err := db.Raw(`SELECT COALESCE(CHARACTER_MAXIMUM_LENGTH, 0) FROM information_schema.columns
	                 WHERE table_schema = DATABASE() AND table_name = ? AND column_name = 'idempotency_key'`,
		table).Scan(&width).Error; err != nil {
		return err
	}
	if width >= idempotencyKeyWidth {
		return nil
	}
	return db.Exec(fmt.Sprintf("ALTER TABLE `%s` MODIFY `idempotency_key` varchar(%d) "+
		"COMMENT '自定义幂等键原文(规范格式 site:stage:<标识>；空=回退 site:stage:url)'",
		table, idempotencyKeyWidth)).Error
}

// ensureIdempotencyHashColumn 加哈希列。**可空**是关键：MySQL 的唯一索引把多个 NULL 视作互不相同，
// 没自定义键的行（占大多数）必须落 NULL，落空串的话全表只能有一行。
func ensureIdempotencyHashColumn(db *gorm.DB, table string) error {
	ok, err := columnExists(db, table, "idempotency_hash")
	if err != nil || ok {
		return err
	}
	return db.Exec(fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `idempotency_hash` char(64) NULL "+
		"COMMENT '幂等键的sha256(NULL=没自定义键；唯一索引建在它上面)'", table)).Error
}

// backfillIdempotencyHash 回填：MySQL 的 SHA2(x,256) 与 Go 的 sha256 小写十六进制是同一个值，
// 所以回填过的老行与新插入的行不会各算各的（与 url_hash 的既有约定一致）。
func backfillIdempotencyHash(db *gorm.DB, table string) error {
	return db.Exec(fmt.Sprintf("UPDATE `%s` SET `idempotency_hash` = SHA2(`idempotency_key`, 256) "+
		"WHERE `idempotency_key` <> '' AND `idempotency_hash` IS NULL", table)).Error
}

// assertNoDuplicateIdempotencyKeys 建唯一索引之前先查重复：直接建的话报的是 1062，
// 现场（哪个键、几行）还得自己再查一遍。这里一次说清，并且**不替业务决定删哪一行**。
func assertNoDuplicateIdempotencyKeys(db *gorm.DB, table string) error {
	var key string
	var n int64
	err := db.Raw(fmt.Sprintf("SELECT `idempotency_key` AS k, COUNT(*) AS n FROM `%s` "+
		"WHERE `idempotency_key` <> '' GROUP BY `idempotency_key` HAVING n > 1 LIMIT 1", table)).
		Row().Scan(&key, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("crawler_tasks 里有重复的自定义幂等键（%q 出现 %d 次），唯一索引建不上去 —— "+
		"先处理这些行（重投或删除其中一条）再跑迁移；删哪一条该由业务决定，迁移不替你做", key, n)
}
