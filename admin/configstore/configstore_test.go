package configstore

import (
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// 表不存在就报错，且报错里点名怎么修 —— 少了这张表白名单会**静默失效**（读不到 = 不限制来源），
// 所以调用方是拿它当 fail fast 用的。
func TestEnsureTable(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(0, 0) // information_schema 里查不到
	err := New(gormfake.MustOpen(f)).EnsureTable()
	if err == nil {
		t.Fatal("表不存在时应当报错")
	}
	for _, want := range []string{"crawler_config", "papa migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报错里缺 %q：%v", want, err)
		}
	}

	ok := gormfake.New()
	ok.SetCounts(1, 1)
	if err := New(gormfake.MustOpen(ok)).EnsureTable(); err != nil {
		t.Fatalf("表存在时不该报错：%v", err)
	}
}

// Load：**行不存在**返回 (nil, false, nil) —— 对白名单来说那是"运营没设过 = 不限制"，
// 与"读出错"（err != nil = 配置系统不可用）必须分开报。
func TestLoad(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	raw, found, err := store.Load(KeyIPWhitelist)
	if err != nil || found || raw != nil {
		t.Fatalf("没有这一行时应是 (nil, false, nil)，实得 (%s, %v, %v)", raw, found, err)
	}

	f.SetRow(map[string]driver.Value{"cfg_key": "ip_whitelist", "cfg_val": []byte(`["10.0.0.0/8"]`)})
	raw, found, err = store.Load(KeyIPWhitelist)
	if err != nil || !found {
		t.Fatalf("实得 found=%v err=%v", found, err)
	}
	if string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("值 = %s", raw)
	}
}

// 查库出错要冒出来（调用方据此记 Error 而不是当成"没配"）。
func TestLoadPropagatesError(t *testing.T) {
	f := gormfake.New()
	f.SetQueryError("crawler_config", "库抖了")
	if _, _, err := New(gormfake.MustOpen(f)).Load(KeyIPWhitelist); err == nil {
		t.Fatal("查库出错时应当报错，而不是当成「没配」")
	}
}

// Save：upsert 一行、带上改它的人。列名与更新列都要钉住 ——
// 尤其 `updated_at`：gorm 的 autoUpdateTime 只在 Update/Updates 那条路上生效，
// 这里走的是手写的 ON DUPLICATE KEY UPDATE，漏了它就永远停在首次插入的时刻。
func TestSave(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	if err := store.Save(KeyIPWhitelist, []string{"10.0.0.0/8", "192.168.1.1"}, "alice"); err != nil {
		t.Fatalf("Save = %v", err)
	}

	w := f.Written()
	for _, want := range []string{
		"INSERT INTO `crawler_config`",
		"ON DUPLICATE KEY UPDATE",
		"`cfg_key`", "`cfg_val`", "`updated_by`", "`updated_at`",
	} {
		if !strings.Contains(w, want) {
			t.Fatalf("SQL 里缺 %q：\n%s", want, w)
		}
	}
	for _, want := range []string{"ip_whitelist", `["10.0.0.0/8","192.168.1.1"]`, "alice"} {
		if !strings.Contains(w, want) {
			t.Fatalf("绑定参数里缺 %q：\n%s", want, w)
		}
	}
}

// 注册表本身要自洽：map 键与 Spec.Key 一致、有说明、Default 能过自己的 Validate。
// 加一项配置时最常犯的错就是复制粘贴改漏一边 —— 这条用例专门逮它。
func TestRegistryIsSelfConsistent(t *testing.T) {
	if len(registry) == 0 {
		t.Fatal("注册表不该是空的")
	}
	for k, sp := range registry {
		if sp.Key != k {
			t.Errorf("注册表键 %q 与 Spec.Key %q 对不上", k, sp.Key)
		}
		if strings.TrimSpace(sp.Doc) == "" {
			t.Errorf("%s 没有 Doc —— 报错文案与文档都从它取", k)
		}
		if sp.Default == nil {
			continue // 没有默认值是允许的
		}
		if sp.Validate != nil {
			if err := sp.Validate(sp.Default); err != nil {
				t.Errorf("%s 的 Default 过不了自己的 Validate：%v", k, err)
			}
		}
	}
	for _, want := range []string{KeyIPWhitelist} {
		if _, ok := registry[want]; !ok {
			t.Errorf("注册表里没有 %s", want)
		}
	}
}

// 未登记的键：读、写、类型化读都要报错，并把已知的键列出来（键名打错时能自己看出来）。
func TestUnknownKeyRejected(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	if _, _, err := store.Load("ip_whitlist"); err == nil { // 少个 e
		t.Fatal("未登记的键读也应当报错")
	} else if !strings.Contains(err.Error(), KeyIPWhitelist) {
		t.Fatalf("报错该列出已知的键：%v", err)
	}
	if err := store.Save("ip_whitlist", []string{"10.0.0.0/8"}, "alice"); err == nil {
		t.Fatal("未登记的键写也应当报错")
	}
	if _, err := store.LoadStrings("whatever"); err == nil {
		t.Fatal("未登记的键类型化读也应当报错")
	}
	if w := f.Written(); w != "" {
		t.Fatalf("未登记的键不该碰库：\n%s", w)
	}
}

// 值的**形状**在写入点拦：写坏一项运行期配置的代价是"限制悄悄放宽"这类安静的事故。
// 只管形状不管业务语义 —— 元素是不是合法 CIDR 由写它的那一层判（见包注释）。
func TestSaveValidatesShape(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	if err := store.Save(KeyIPWhitelist, "10.0.0.0/8", "alice"); err == nil {
		t.Fatal("字符串（而不是字符串数组）应当被拒")
	}
	if err := store.Save(KeyIPWhitelist, []any{"10.0.0.0/8", 42}, "alice"); err == nil {
		t.Fatal("数组里混了非字符串应当被拒")
	}
	if err := store.Save(KeyIPWhitelist, []any{"10.0.0.0/8"}, "alice"); err != nil {
		t.Fatalf("JSON 解出来的 []any（全是字符串）应当被接受：%v", err)
	}
	if err := store.Save(KeyIPWhitelist, []string{"10.0.0.0/8"}, "alice"); err != nil {
		t.Fatalf("[]string 应当被接受：%v", err)
	}
}

// LoadStrings 给的是"这一项**生效的值**"：行不存在 → 规格里的 Default（空数组 = 不限制）；
// 值不是字符串数组 → 报错（由调用方按不限制来源继续，那是它的业务语义）。
func TestLoadStrings(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	got, err := store.LoadStrings(KeyIPWhitelist)
	if err != nil {
		t.Fatalf("行不存在不该报错：%v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("行不存在时应给 Default（非 nil 空数组），实得 %v", got)
	}

	f.SetRow(map[string]driver.Value{"cfg_key": "ip_whitelist", "cfg_val": []byte(`["10.0.0.0/8","192.168.1.1"]`)})
	got, err = store.LoadStrings(KeyIPWhitelist)
	if err != nil || len(got) != 2 {
		t.Fatalf("实得 %v, err=%v", got, err)
	}

	f2 := gormfake.New()
	f2.SetRow(map[string]driver.Value{"cfg_key": "ip_whitelist", "cfg_val": []byte(`{"a":1}`)})
	if _, err := New(gormfake.MustOpen(f2)).LoadStrings(KeyIPWhitelist); err == nil {
		t.Fatal("值不是字符串数组时应当报错")
	}
}
