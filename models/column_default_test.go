package models

import (
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 真库才看得见的一类坑：**gorm 对"带 `default:` 的非指针字段 + 零值"会把零值替换成默认值**
// （结构体与写库的值都被改掉）。于是 `AutoRepeat: &false` 这种显式声明会被静默写成 true，
// 站点第二次启动按"库为事实"就把自动轮询开起来了 —— 与声明的意思正好相反，而且不报错。
//
// 这里用 DryRun 钉住"显式写的 false 就该是 false"（假库不跑 gorm 的默认值逻辑，测不出来）。
func TestCreateKeepsExplicitFalse(t *testing.T) {
	db := dryRun(t)

	site := CrawlerSite{Key: "a", BaseURL: "https://x/", AutoRepeat: false}
	if err := db.Create(&site).Error; err != nil {
		t.Fatalf("create site: %v", err)
	}
	if site.AutoRepeat {
		t.Error("CrawlerSite.AutoRepeat 显式传 false，写完后被改成了 true —— 站点声明的 &false 会被静默反转")
	}

	token := AccessToken{Operator: "alice", TokenHash: strings.Repeat("a", 64), Enabled: false}
	if err := db.Create(&token).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}
	if token.Enabled {
		t.Error("AccessToken.Enabled 显式传 false，写完后被改成了 true")
	}
}

// dryRun 只借 mysql 方言拼 SQL，**不连库**（与 engine/sqlshape_test.go 同一套做法）。
func dryRun(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "root@tcp(127.0.0.1:9)/none?charset=utf8mb4&parseTime=True",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DryRun: true, DisableAutomaticPing: true,
		// 必须关默认事务：gorm 默认把写包在事务里，而 Begin() 会去连库。
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	})
	if err != nil {
		t.Fatalf("open dry-run mysql: %v", err)
	}
	return db
}
