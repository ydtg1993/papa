package tokenadmin

import (
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// DBStore 的读写路径（List / Create / SetEnabled / Delete / whyRejected）。
//
// 以前这几条只有"SQL 形态"由 DryRun 钉住（store_test.go），**结果**那一层没人验：
// 条件更新影响 0 行时该翻成哪个错误、Create 有没有把操作人存进去 —— 都是靠读代码相信的。
// 这里用 internal/gormfake 直接把"影响 0 行""查不到行"这些情况造出来。
func TestDBStoreList(t *testing.T) {
	f := gormfake.New()
	f.SetRow(map[string]driver.Value{
		"id": int64(7), "operator": "alice", "enabled": true, "note": "运维",
		"created_at": time.Now(), "updated_at": time.Now(),
	})
	store := NewStore(gormfake.MustOpen(f))

	tokens, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tokens) != 1 || tokens[0].ID != 7 || tokens[0].Operator != "alice" {
		t.Fatalf("List = %+v", tokens)
	}
	// 哈希不进内存、更不会顺着接口出去：取列清单里不该有 token_hash
	if sql := f.Written(); strings.Contains(sql, "token_hash") {
		t.Fatalf("List 不该把 token_hash 取出来：%s", sql)
	}

	f.SetQueryError("access_token", "db down")
	if _, err := store.List(); err == nil {
		t.Fatal("查库出错应当报错，不是静默给空列表")
	}
}

func TestDBStoreCreate(t *testing.T) {
	f := gormfake.New()
	f.SetLastInsertID(42)
	store := NewStore(gormfake.MustOpen(f))

	token, id, err := store.Create("  alice  ", "  运维  ")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" || id != 42 {
		t.Fatalf("Create = (%q, %d)", token, id)
	}
	// 明文只回给调用方一次，库里存的是 sha256 —— 写下去的语句里不该出现明文
	if sql := f.Written(); strings.Contains(sql, token) {
		t.Fatalf("入库语句里出现了明文令牌：%s", sql)
	}
	// 空白要裁掉（操作人与备注都是）
	if sql := f.Written(); !strings.Contains(sql, "alice") || strings.Contains(sql, "  alice") {
		t.Fatalf("操作人应当裁掉空白再入库：%s", sql)
	}

	if _, _, err := store.Create("   ", ""); !errors.Is(err, ErrOperatorRequired) {
		t.Fatalf("空操作人应当报 ErrOperatorRequired，实得 %v", err)
	}

	f.SetExecError("db down")
	if _, _, err := store.Create("bob", ""); err == nil {
		t.Fatal("写库出错应当报错")
	}
}

func TestDBStoreSetEnabled(t *testing.T) {
	f := gormfake.New()
	f.SetAffected(1)
	store := NewStore(gormfake.MustOpen(f))
	if err := store.SetEnabled(7, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if sql := f.Written(); !strings.Contains(sql, "enabled") {
		t.Fatalf("应当写 enabled 列：%s", sql)
	}

	// 影响 0 行 = 条件没匹配上（并发改过 / 状态不符）→ 再查一次把原因说清
	f.SetAffected(0)
	f.SetRow(map[string]driver.Value{"id": int64(7)})
	if err := store.SetEnabled(7, false); !errors.Is(err, ErrChanged) {
		t.Fatalf("行还在但状态被改过 → ErrChanged，实得 %v", err)
	}
	f.SetRow(nil) // 查不到 → 行不存在
	if err := store.SetEnabled(7, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("行不存在 → ErrNotFound，实得 %v", err)
	}
	f.SetExecError("db down")
	if err := store.SetEnabled(7, false); err == nil {
		t.Fatal("写库出错应当报错")
	}
}

func TestDBStoreDelete(t *testing.T) {
	f := gormfake.New()
	f.SetAffected(1)
	store := NewStore(gormfake.MustOpen(f))
	if err := store.Delete(7); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// 影响 0 行：行不存在 → ErrNotFound；行还在（最后一条被守卫拦下）→ ErrLastToken
	f.SetAffected(0)
	f.SetRow(nil)
	if err := store.Delete(7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("行不存在 → ErrNotFound，实得 %v", err)
	}
	f.SetRow(map[string]driver.Value{"id": int64(7)})
	if err := store.Delete(7); !errors.Is(err, ErrLastToken) {
		t.Fatalf("行还在（最后一条被守卫拦下）→ ErrLastToken，实得 %v", err)
	}
	f.SetExecError("db down")
	if err := store.Delete(7); err == nil {
		t.Fatal("写库出错应当报错")
	}
}
