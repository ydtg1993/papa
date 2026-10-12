package database

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
)

// 这次迁移要动**生产库**的表结构（删索引、改列类型、回填、建新索引），而顺序错了就是事故：
// 先建唯一索引再回填的话，一堆空串会自己撞自己；不先删旧索引的话，url 改 text 会被
// MySQL 拒绝（TEXT 列不带前缀长度进不了索引）。所以这里把"库当前长什么样 → 该发哪几条 DDL、
// 什么顺序"钉死。
//
// 用的还是 upsert_test.go 里那套假驱动（加了 queryFn 钩子）：守卫查询形状相同，
// 靠参数区分问的是哪张表 / 哪个列 / 哪个索引。

// schemaState 假的"库现状"。
type schemaState struct {
	table       bool   // crawler_tasks 在不在（全新库 = false）
	urlType     string // url 列的类型（DATA_TYPE）
	hasURLHash  bool   // url_hash 列在不在
	hasOldIndex bool   // 旧唯一索引 idx_stage_url
	hasNewIndex bool   // 新唯一索引 idx_stage_url_hash
	// missingHash 还有多少行等着回填；urlHashNullable = url_hash 现在可空吗
	missingHash     int64
	urlHashNullable bool
	// v3.3.2 起结果落业务表：crawler_tasks 的这两列由显式迁移删掉（见 migrate_taskcolumns.go）
	hasTitle   bool
	hasContent bool
}

// schemaStub 造一个按 st 回答守卫查询的假库；返回的指针可改（用来模拟"迁移已经生效"）。
func schemaStub(t *testing.T, st *schemaState) *stubDB {
	t.Helper()
	s := newStubDB()
	s.queryFn = func(q string, args []driver.Value) ([]string, [][]driver.Value) {
		low := strings.ToLower(q)
		arg := func(i int) string {
			if i < len(args) {
				return fmt.Sprint(args[i])
			}
			return ""
		}
		count := func(v bool) ([]string, [][]driver.Value) {
			n := int64(0)
			if v {
				n = 1
			}
			return []string{"COUNT(*)"}, [][]driver.Value{{n}}
		}
		last := func() string { return arg(len(args) - 1) }

		switch {
		case strings.Contains(low, "information_schema.tables"):
			return count(st.table)
		case strings.Contains(low, "information_schema.statistics"):
			switch last() {
			case "idx_stage_url":
				return count(st.hasOldIndex)
			case "idx_stage_url_hash":
				return count(st.hasNewIndex)
			}
		case strings.Contains(low, "information_schema.columns"):
			if strings.Contains(low, "is_nullable") {
				yes := "NO"
				if st.urlHashNullable {
					yes = "YES"
				}
				return []string{"IS_NULLABLE"}, [][]driver.Value{{yes}}
			}
			if strings.Contains(low, "data_type") {
				return []string{"DATA_TYPE"}, [][]driver.Value{{st.urlType}}
			}
			if last() == "url_hash" {
				return count(st.hasURLHash)
			}
			if last() == "title" {
				return count(st.hasTitle)
			}
			if last() == "content" {
				return count(st.hasContent)
			}
		case strings.Contains(low, "count(*) from `crawler_tasks`") && strings.Contains(low, "url_hash"):
			return []string{"COUNT(*)"}, [][]driver.Value{{st.missingHash}}
		}
		t.Fatalf("迁移发了没预期的查询：%s %v", q, args)
		return nil, nil
	}
	return s
}

// 迁移步骤的**先后顺序**：删旧索引 → 加列 → url 改 text → 回填 → 收紧 NOT NULL → 建新索引。
func TestMigrateTaskURLHashOrderOnLegacySchema(t *testing.T) {
	st := &schemaState{
		table: true, urlType: "varchar", hasOldIndex: true,
		missingHash: 66, urlHashNullable: true, // 老库：66 行待回填、列还空着
	}
	s := schemaStub(t, st)
	db := openStubDB(t, s)

	if err := migrateTaskURLHash(db); err != nil {
		t.Fatalf("migrateTaskURLHash = %v", err)
	}

	writes := s.written()
	wantOrder := []string{
		"DROP INDEX `idx_stage_url`",               // ① 旧索引必须先进垃圾桶
		"ADD COLUMN `url_hash`",                    // ② 先可空（表里有行）
		"MODIFY `url` text NOT NULL",               // ③ url 改 text（索引已经让开了）
		"SHA2(`url`, 256)",                         // ④ 回填
		"MODIFY `url_hash` char(64) NOT NULL",      // ⑤ 回填完才收紧
		"CREATE UNIQUE INDEX `idx_stage_url_hash`", // ⑥ 最后才建唯一索引
	}
	pos := -1
	for _, w := range wantOrder {
		i := strings.Index(writes, w)
		if i < 0 {
			t.Fatalf("迁移没发这条语句 %q，实得：\n%s", w, writes)
		}
		if i < pos {
			t.Fatalf("顺序不对：%q 出现在前一条之前。完整语句：\n%s", w, writes)
		}
		pos = i
	}
}

// 全新库（表都还没有）：整段跳过 —— AutoMigrate 会按新结构建表。
func TestMigrateTaskURLHashSkipsFreshDatabase(t *testing.T) {
	st := &schemaState{table: false}
	s := schemaStub(t, st)
	db := openStubDB(t, s)

	if err := migrateTaskURLHash(db); err != nil {
		t.Fatalf("migrateTaskURLHash = %v", err)
	}
	if got := s.written(); got != "" {
		t.Fatalf("表不存在时不该发任何 DDL，实得：\n%s", got)
	}
}

// 已经迁移过的库：什么都不做（可重复跑是硬要求 —— 每次 `papa migrate` 都会走一遍）。
func TestMigrateTaskURLHashIsIdempotent(t *testing.T) {
	st := &schemaState{
		table: true, urlType: "varchar", hasOldIndex: true,
		missingHash: 66, urlHashNullable: true,
	}
	s := schemaStub(t, st)
	db := openStubDB(t, s)

	if err := migrateTaskURLHash(db); err != nil {
		t.Fatalf("第一次 = %v", err)
	}
	first := s.written()
	if first == "" {
		t.Fatal("第一次应当真的做点事")
	}

	// 模拟"迁移已经生效"
	*st = schemaState{table: true, urlType: "text", hasURLHash: true, hasNewIndex: true}
	before := len(s.execs)
	if err := migrateTaskURLHash(db); err != nil {
		t.Fatalf("第二次 = %v", err)
	}
	if extra := s.execs[before:]; len(extra) != 0 {
		t.Fatalf("第二次不该再发 DDL，实得：%v", extra)
	}
}

// title / content 两列的显式删除：模型里把字段删掉，AutoMigrate **什么都不做**（只增不减），
// 所以库里的两列只能这样显式删 —— 留着它们，后台任务表的搜索声明一旦再带上 title 就是真库 1054。
func TestDropLegacyTaskColumns(t *testing.T) {
	// ① 两列都在：各发一条 DROP COLUMN
	st := &schemaState{table: true, hasTitle: true, hasContent: true}
	s := schemaStub(t, st)
	if err := dropLegacyTaskColumns(openStubDB(t, s)); err != nil {
		t.Fatalf("dropLegacyTaskColumns = %v", err)
	}
	writes := s.written()
	for _, want := range []string{"DROP COLUMN `title`", "DROP COLUMN `content`"} {
		if !strings.Contains(writes, want) {
			t.Fatalf("应发 %q，实得：\n%s", want, writes)
		}
	}

	// ② 列已经不在了（重复跑 / 已迁移过的库）：一条 DDL 都不发 —— 迁移必须可重复跑
	st2 := &schemaState{table: true}
	s2 := schemaStub(t, st2)
	if err := dropLegacyTaskColumns(openStubDB(t, s2)); err != nil {
		t.Fatalf("dropLegacyTaskColumns（列已删过）= %v", err)
	}
	if w := s2.written(); strings.Contains(w, "ALTER") {
		t.Fatalf("列不存在时不该发 DDL：\n%s", w)
	}

	// ③ 表都不在（全新库）：整段跳过，交给后面的 AutoMigrate 按新结构建表
	st3 := &schemaState{table: false}
	s3 := schemaStub(t, st3)
	if err := dropLegacyTaskColumns(openStubDB(t, s3)); err != nil {
		t.Fatalf("dropLegacyTaskColumns（表不存在）= %v", err)
	}
	if w := s3.written(); strings.Contains(w, "ALTER") {
		t.Fatalf("表不存在时不该发 DDL：\n%s", w)
	}
}
