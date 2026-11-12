package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 引擎测试跑的是假库（不解析 SQL），所以"这条 SQL 能不能被 MySQL 认"这一层以前只能等真跑才发现 ——
// 那个坑就是这么漏过去的：**`repeat`（crawler_tasks）是 MySQL 保留字**，裸写进 SQL 直接被当成
// 关键字 / 函数，整条语句 Error 1064（在真库上逐条验过）：
//
//	SELECT repeat + 1 ...  → Error 1064     SELECT `repeat` + 1 ...  → ok
//
// （另一半历史：`key` 曾经也是（crawler_sites 那一列）—— 那张表已经并进 `crawler_config` 的 `sites` 键，
// 这个坑随表一起消失了。扫描式的守卫见 sqlreserved_test.go。）
//
// 这里用 gorm 的 DryRun（**不连库**，只借 mysql 方言把语句拼出来）钉住生产路径那条语句的列名带引号。
// 往裸 SQL 里再加列名时，这条会提醒一句"保留字要引号"。
func TestReservedWordColumnsAreQuotedInRawSQL(t *testing.T) {
	db := dryRunMySQL(t)

	t.Run("提交可轮询任务：幂等初始化 repeat", func(t *testing.T) {
		sql := renderSQL(t, db, func(tx *gorm.DB) *gorm.DB {
			return tx.Model(&models.CrawlerTask{}).Where("id = ?", 1).
				Updates(submitUpdates(&Task{ID: 1, Repeatable: true}))
		})
		if !strings.Contains(sql, "`repeat`=CASE WHEN `repeat` = 0 THEN 1 ELSE `repeat` END") {
			t.Errorf("`repeat` 是保留字，裸写会 1064；拼出来应当是带引号的：%s", sql)
		}
	})
}

func TestRepeatResetAtomicallyAdvancesQuotedRound(t *testing.T) {
	db := dryRunMySQL(t)
	var sql string
	if err := db.Callback().Update().After("gorm:update").Register("test:repeat_reset_sql", func(tx *gorm.DB) {
		sql = tx.Statement.SQL.String()
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.RepeatQueue.Interval = time.Hour
	e := &Engine{db: db, cfg: cfg, stages: map[string]*stageInfo{
		"stub:stub": {stage: "stub", config: StageConfig{Site: "stub", RepeatLimit: 13}},
	}}
	row := actionRow(7, "stub")
	row.Repeat, row.Repeatable, row.Status = 12, models.RepeatableYes, models.TaskStatusSuccess
	// DryRun reports zero affected rows, so the production method renders its
	// guarded reset then returns without enqueuing or emitting events.
	if e.requeueRepeatTaskIn(row) {
		t.Fatal("dry run unexpectedly enqueued")
	}
	for _, fragment := range []string{"`repeat`=`repeat` + 1", "`status`=?", "`last_repeat_at`=NOW()", "`next_repeat_at`=FROM_UNIXTIME", "repeatable = ? AND status IN", "`repeat` < ?"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("missing atomic reset fragment %q: %s", fragment, sql)
		}
	}
}

// 去重查键必须**与唯一索引的列一致**：查的键与索引的键不一致时，插入撞索引后的回查会空手而归，
// 把并发冲突报成"插入失败"（幂等键文本改过的那种情况最容易踩）。
// 假库不解析 WHERE，所以这条用 DryRun 把生产路径那两条语句拼出来看。
func TestDedupScopeMatchesUniqueIndexes(t *testing.T) {
	db := dryRunMySQL(t)
	e := &Engine{db: db}
	for _, resourceID := range []string{"", "42"} {
		task := &Task{URL: "https://example.com/a", Stage: "catalog", Site: "shop", ResourceID: resourceID}
		var rec models.CrawlerTask
		sql := renderSQL(t, db, func(*gorm.DB) *gorm.DB { return e.dedupScope(task).First(&rec) })
		if !strings.Contains(sql, "site = ? AND stage = ? AND identity_hash = ?") || strings.Contains(sql, "url_hash") || strings.Contains(sql, "idempotency_hash") {
			t.Fatalf("wrong identity predicate: %s", sql)
		}
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
