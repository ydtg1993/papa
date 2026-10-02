package crawler

import (
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// dryDB 造一个不会真的连库的 DB：DryRun + 跳过版本探测 + 关掉 Open 后的自动 Ping。
func dryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:3306)/x",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

// 并发保护的关键是「条件写在 UPDATE 语句本身」而不是先查再写。
// 这里断言的就是 production 里那几份 scope 函数（不是复制品）——
// RowsAffected 的分支需要真库，离线只能钉到语句这一层。
func TestAdminScopesCarryConditions(t *testing.T) {
	db := dryDB(t)

	cases := []struct {
		name string
		run  func() string
		want []string
	}{
		{
			"认领：只有待处理能被认领",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return claimScope(tx, 7).Update("status", models.TaskStatusProcessing)
				})
			},
			[]string{"UPDATE", "id = 7", "status = 0"},
		},
		{
			"重投：排除处理中 + reprocess 版本条件",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return retryScope(tx, 7, 3).Updates(map[string]any{
						"status":    models.TaskStatusPending,
						"reprocess": gorm.Expr("reprocess + 1"),
					})
				})
			},
			[]string{"UPDATE", "id = 7", "status <> 1", "reprocess = 3"},
		},
		{
			"标失败：只碰非终态，原因写进错误列",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return markFailedScope(tx, 7).Updates(map[string]any{
						"status": models.TaskStatusFailed,
						"error":  gorm.Expr("CONCAT(COALESCE(error, ''), ?)", "后台手动标记失败：内容违规\n"),
					})
				})
			},
			[]string{"UPDATE", "id = 7", "status IN", "CONCAT", "内容违规"},
		},
		{
			"删除：排除处理中",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return deleteScope(tx, 7).Delete(&models.CrawlerTask{})
				})
			},
			[]string{"DELETE", "id = 7", "status <> 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql := tc.run()
			for _, w := range tc.want {
				if !strings.Contains(sql, w) {
					t.Fatalf("SQL 里缺少 %q：\n%s", w, sql)
				}
			}
		})
	}
}
