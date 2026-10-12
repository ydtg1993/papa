package database

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
)

// 幂等键硬化要动生产库的列宽与索引，且**顺序错了就是事故**：
// 不先删键原文上的旧索引，加宽到 varchar(1000) 会被 InnoDB 的 3072 字节键长上限拦下（真库 1071）；
// 不回填就建唯一索引，一列 NULL 里会蹦出重复；有重复键时不预先查出来，报的是 1062 而不是"哪个键重复几次"。
// 这里把"库现在长什么样 → 该发哪几条 DDL、什么顺序"钉死。

// keyState 假的"库现状"。
type keyState struct {
	table        bool   // crawler_tasks 在不在（全新库 = false）
	keyWidth     int64  // idempotency_key 的字符宽度（老库 500）
	hasHashCol   bool   // idempotency_hash 列在不在
	hasLegacyIdx bool   // 键原文上的旧普通索引 idx_crawler_tasks_idempotency_key
	hasHashIdx   bool   // 新的唯一索引 idx_idempotency_hash
	dupKey       string // 若非空：库里存在这个重复键
	dupCount     int64
}

func keyStub(t *testing.T, st *keyState) *stubDB {
	t.Helper()
	s := newStubDB()
	s.queryFn = func(q string, args []driver.Value) ([]string, [][]driver.Value) {
		low := strings.ToLower(q)
		last := func() string {
			if len(args) > 0 {
				return fmt.Sprint(args[len(args)-1])
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
		switch {
		case strings.Contains(low, "information_schema.tables"):
			return count(st.table)
		case strings.Contains(low, "information_schema.statistics"):
			switch last() {
			case legacyIdempotencyKeyIndex:
				return count(st.hasLegacyIdx)
			case idempotencyHashIndex:
				return count(st.hasHashIdx)
			}
		case strings.Contains(low, "character_maximum_length"):
			return []string{"CHARACTER_MAXIMUM_LENGTH"}, [][]driver.Value{{st.keyWidth}}
		case strings.Contains(low, "information_schema.columns"):
			return count(st.hasHashCol)
		case strings.Contains(low, "having n > 1"):
			if st.dupKey == "" {
				return []string{"k", "n"}, nil
			}
			return []string{"k", "n"}, [][]driver.Value{{st.dupKey, st.dupCount}}
		}
		t.Fatalf("迁移发了没预期的查询：%s %v", q, args)
		return nil, nil
	}
	return s
}

// 老库：键原文还是 varchar(500)、带旧普通索引、没有哈希列。
// 期望顺序：删旧索引 → 加宽 → 加哈希列 →（**查重**，它是 SELECT、不进 DDL 日志）→ 回填 → 建唯一索引。
// 查重必须排在回填之前，理由见重复用例。
func TestMigrateIdempotencyKeyOrderOnLegacySchema(t *testing.T) {
	st := &keyState{table: true, keyWidth: 500, hasLegacyIdx: true}
	s := keyStub(t, st)
	if err := migrateIdempotencyKey(openStubDB(t, s)); err != nil {
		t.Fatalf("migrateIdempotencyKey = %v", err)
	}

	writes := s.written()
	wantOrder := []string{
		"DROP INDEX `" + legacyIdempotencyKeyIndex + "`", // ① 必须先进垃圾桶，否则下面加宽会 1071
		"MODIFY `idempotency_key` varchar(1000)",         // ② 加宽
		"ADD COLUMN `idempotency_hash` char(64) NULL",    // ③ 加哈希列（必须可空）
		"SHA2(`idempotency_key`, 256)",                   // ④ 回填（与 Go 侧同一个值）
		"CREATE UNIQUE INDEX `" + idempotencyHashIndex + "`",
	}
	pos := -1
	for _, w := range wantOrder {
		i := strings.Index(writes, w)
		if i < 0 {
			t.Fatalf("迁移没发这条语句 %q，实得：\n%s", w, writes)
		}
		if i < pos {
			t.Fatalf("语句顺序不对：%q 出现在更早的语句之前\n%s", w, writes)
		}
		pos = i
	}
}

// 已迁移过的库（宽度够、哈希列在、索引在、旧索引已删）：一条 DDL 都不发 —— 迁移必须可重复跑。
func TestMigrateIdempotencyKeyIdempotent(t *testing.T) {
	st := &keyState{table: true, keyWidth: 1000, hasHashCol: true, hasHashIdx: true}
	s := keyStub(t, st)
	if err := migrateIdempotencyKey(openStubDB(t, s)); err != nil {
		t.Fatalf("migrateIdempotencyKey = %v", err)
	}
	if w := s.written(); strings.Contains(w, "ALTER") || strings.Contains(w, "CREATE UNIQUE INDEX") {
		t.Fatalf("已迁移过的库不该再发 DDL：\n%s", w)
	}
}

// 有重复的非空键：报错要说清是哪个键、几次，且**不发**那条注定 1062 的 CREATE UNIQUE INDEX。
func TestMigrateIdempotencyKeyRejectsDuplicates(t *testing.T) {
	st := &keyState{table: true, keyWidth: 1000, hasHashCol: true, dupKey: "shop:catalog:https://x/", dupCount: 3}
	s := keyStub(t, st)
	err := migrateIdempotencyKey(openStubDB(t, s))
	if err == nil {
		t.Fatal("有重复键时应当报错（而不是让 CREATE UNIQUE INDEX 以 1062 收场）")
	}
	if !strings.Contains(err.Error(), "shop:catalog:https://x/") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("报错要带上重复的键与次数：%v", err)
	}
	if strings.Contains(s.written(), "CREATE UNIQUE INDEX") {
		t.Fatalf("重复没处理掉之前不该建唯一索引：\n%s", s.written())
	}
	// 查重排在**回填之前**：回填是把重复写进哈希列，唯一索引一旦已存在（重跑、或上一轮迁移失败留下的），
	// 那条 UPDATE 自己就 1062，报出来是一句看不懂的 "Duplicate entry"。所以这里断言回填根本没跑。
	if strings.Contains(s.written(), "SHA2(`idempotency_key`, 256)") {
		t.Fatalf("有重复键时不该回填（回填会先撞 1062，把错误盖成人话）：\n%s", s.written())
	}
}

// 表都不在（全新库）：整段跳过，交给 AutoMigrate 按模型建。
func TestMigrateIdempotencyKeySkipsFreshDB(t *testing.T) {
	s := keyStub(t, &keyState{table: false})
	if err := migrateIdempotencyKey(openStubDB(t, s)); err != nil {
		t.Fatalf("migrateIdempotencyKey（全新库）= %v", err)
	}
	if w := s.written(); strings.Contains(w, "ALTER") {
		t.Fatalf("表不存在时不该发 DDL：\n%s", w)
	}
}
