package tokenadmin

import (
	"errors"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

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

// 停用/启用的条件必须写在 UPDATE 里（当前状态得是"另一头"），删除则按主键。
func TestWriteConditionsInStatement(t *testing.T) {
	db := dryDB(t)

	// 断言的是 production 那份条件（enabledScope/deleteScope），不是复制品
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return enabledScope(tx, 7, false).Update("enabled", false)
	})
	for _, want := range []string{"UPDATE", "id = 7", "enabled"} {
		if !strings.Contains(sql, want) {
			t.Errorf("停用 SQL 缺少 %q：\n%s", want, sql)
		}
	}

	del := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return deleteScope(tx, 7).Delete(&models.AccessToken{})
	})
	if !strings.Contains(del, "DELETE") || !strings.Contains(del, "id = 7") {
		t.Errorf("删除 SQL 不对：\n%s", del)
	}
}

// 列表只取页面需要的列：哈希不能顺着接口出去。
func TestListDoesNotSelectHash(t *testing.T) {
	sql := dryDB(t).ToSQL(func(tx *gorm.DB) *gorm.DB {
		return tx.Model(&models.AccessToken{}).Select(tokenColumns).Order("id DESC").Find(&[]Token{})
	})
	if strings.Contains(sql, "token_hash") {
		t.Errorf("列表 SQL 不该查 token_hash：\n%s", sql)
	}
	for _, want := range []string{"operator", "enabled", "note", "created_at"} {
		if !strings.Contains(sql, want) {
			t.Errorf("列表 SQL 缺少 %q：\n%s", want, sql)
		}
	}
}

// 操作人为空必须在**碰库之前**就拒掉（空操作人的令牌没有意义，还会把审计搞成无主）。
func TestStoreCreateRejectsEmptyOperator(t *testing.T) {
	for _, op := range []string{"", "   ", "\t\n"} {
		_, _, err := NewStore(dryDB(t)).Create(op, "备注")
		if !errors.Is(err, ErrOperatorRequired) {
			t.Errorf("operator=%q 应返回 ErrOperatorRequired，得到 %v", op, err)
		}
	}
}
