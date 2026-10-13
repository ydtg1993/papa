package database

import (
	"strings"
	"testing"
)

// "默认 scope"（crawler_sites 里空 key 那一行）的显式删除：站点成为必填维度后它没有意义了，
// 留着只会让后台一直多一个「默认 scope」Tab（seedSiteRows 只按声明播种，不会再去动它）。
func TestDeleteDefaultScopeSiteRow(t *testing.T) {
	// ① 表在：发一条 DELETE，且 `key` 要带反引号（它是 MySQL 保留字，裸写就是 Error 1064）
	st := &schemaState{table: true}
	s := schemaStub(t, st)
	if err := deleteDefaultScopeSiteRow(openStubDB(t, s)); err != nil {
		t.Fatalf("deleteDefaultScopeSiteRow = %v", err)
	}
	writes := s.written()
	if !strings.Contains(writes, "DELETE FROM `crawler_sites` WHERE `key` = ''") {
		t.Fatalf("应当删掉空 key 那一行（列名要带反引号），实得：\n%s", writes)
	}

	// ② 再跑一次：还是那条 DELETE（影响 0 行即可），不该报错 —— 迁移必须可重复跑
	before := len(s.execs)
	if err := deleteDefaultScopeSiteRow(openStubDB(t, s)); err != nil {
		t.Fatalf("重复跑 = %v", err)
	}
	if extra := s.execs[before:]; len(extra) != 1 {
		t.Fatalf("重复跑应当只发那一条 DELETE，实得：%v", extra)
	}

	// ③ 表都不在（全新库）：一条 SQL 都不发，交给 AutoMigrate 按模型建表
	st3 := &schemaState{table: false}
	s3 := schemaStub(t, st3)
	if err := deleteDefaultScopeSiteRow(openStubDB(t, s3)); err != nil {
		t.Fatalf("deleteDefaultScopeSiteRow（表不存在）= %v", err)
	}
	if w := s3.written(); w != "" {
		t.Fatalf("表不存在时不该发任何 SQL：\n%s", w)
	}
}
