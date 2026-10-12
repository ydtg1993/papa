package engine

import (
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 引擎测试跑的是假库（不解析 SQL），所以"这条 SQL 能不能被 MySQL 认"这一层以前只能等真跑才发现 ——
// 两个坑就是这么漏过去的：**`repeat`（crawler_tasks）与 `key`（crawler_sites）都是 MySQL 保留字**，
// 裸写进 SQL 直接被当成关键字 / 函数，整条语句 Error 1064（在真库上逐条验过）：
//
//	SELECT repeat + 1 ...             → Error 1064     SELECT `repeat` + 1 ...   → ok
//	SELECT count(*) ... WHERE key='x' → Error 1064     ... WHERE `key` = 'x'     → ok
//
// 这里用 gorm 的 DryRun（**不连库**，只借 mysql 方言把语句拼出来）钉住生产路径那两条语句的列名带引号。
// 往裸 SQL 里再加列名时，这条会提醒一句"保留字要引号"。
func TestReservedWordColumnsAreQuotedInRawSQL(t *testing.T) {
	db := dryRunMySQL(t)

	t.Run("提交可轮询任务：repeat 自增", func(t *testing.T) {
		sql := renderSQL(t, db, func(tx *gorm.DB) *gorm.DB {
			return tx.Model(&models.CrawlerTask{}).Where("id = ?", 1).
				Updates(submitUpdates(&Task{ID: 1, Repeatable: true}))
		})
		if !strings.Contains(sql, "`repeat`=`repeat` + 1") {
			t.Errorf("`repeat` 是保留字，裸写会 1064；拼出来应当是带引号的：%s", sql)
		}
	})

	t.Run("站点行：key 作为条件", func(t *testing.T) {
		sql := renderSQL(t, db, func(tx *gorm.DB) *gorm.DB {
			return siteKeyScope(tx, "huangguo").Updates(map[string]any{"base_url": "https://x/"})
		})
		if !strings.Contains(sql, "`key` = ?") {
			t.Errorf("`key` 是保留字，裸写会 1064；拼出来应当是带引号的：%s", sql)
		}
	})
}

// 去重查键必须**与唯一索引的列一致**：查的键与索引的键不一致时，插入撞索引后的回查会空手而归，
// 把并发冲突报成"插入失败"（幂等键文本改过的那种情况最容易踩）。
// 假库不解析 WHERE，所以这条用 DryRun 把生产路径那两条语句拼出来看。
func TestDedupScopeMatchesUniqueIndexes(t *testing.T) {
	db := dryRunMySQL(t)
	e := &Engine{db: db}

	// 有自定义键 → 按 idempotency_hash 查（键原文 varchar(1000) 在 utf8mb4 下建不了索引）
	withKey := &Task{URL: "https://example.com/a", Stage: "catalog", Site: "shop",
		IdempotencyKey: "shop:catalog:https://example.com/a"}
	var rec models.CrawlerTask
	// 加个 finisher 才会把 SQL 拼出来（DryRun 下只是拼、不执行）
	sql := renderSQL(t, db, func(*gorm.DB) *gorm.DB { return e.dedupScope(withKey).First(&rec) })
	if !strings.Contains(sql, "idempotency_hash = ?") {
		t.Fatalf("有自定义键时应当按 idempotency_hash 查：%s", sql)
	}
	if strings.Contains(sql, "idempotency_key = ?") {
		t.Fatalf("键原文上没有唯一索引，查它在插冲突后会空手而归：%s", sql)
	}

	// 没有自定义键 → 按 (url_hash, stage) 查（idx_stage_url_hash）
	plain := &Task{URL: "https://example.com/a", Stage: "catalog", Site: "shop"}
	sqlPlain := renderSQL(t, db, func(*gorm.DB) *gorm.DB { return e.dedupScope(plain).First(&rec) })
	if !strings.Contains(sqlPlain, "url_hash = ? AND stage = ?") {
		t.Fatalf("没有自定义键时应当按 (url_hash, stage) 查：%s", sqlPlain)
	}
}

// dryRunMySQL 只借 mysql 方言拼 SQL，**不连库**（与 app 层 noDBEngine 同一套做法）。
func dryRunMySQL(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "root@tcp(127.0.0.1:9)/none?charset=utf8mb4&parseTime=True",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun: true, DisableAutomaticPing: true,
		// **必须关掉默认事务**：gorm 默认把每次写包在事务里，而 Begin() 会去连库 ——
		// 那一步在 DryRun 之前发生，连不上就直接失败、SQL 一个字节都拼不出来。
		SkipDefaultTransaction: true,
		Logger:                 gormlogger.Discard,
	})
	if err != nil {
		t.Fatalf("open dry-run mysql: %v", err)
	}
	return db
}

func renderSQL(t *testing.T, db *gorm.DB, build func(*gorm.DB) *gorm.DB) string {
	t.Helper()
	tx := build(db) // DryRun 在 gorm.Config 里开着：只拼 SQL，不执行（所以不连库）
	if tx.Error != nil {
		t.Fatalf("拼 SQL: %v", tx.Error)
	}
	return tx.Statement.SQL.String()
}
