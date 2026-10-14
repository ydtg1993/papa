package configstore

import (
	"database/sql/driver"
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// 缓存层（cache.go）的行为：命中不查库 · 懒加载只查一次 · 读不到不进缓存 ·
// 写先落库再更新内存 · **库写失败不阻断内存更新**（控制面动作的取舍）。

// selects 数一数假库到目前为止执行了几条 SELECT（Load 走它）。
func selects(f *gormfake.DB) int {
	return strings.Count(strings.ToUpper(f.Written()), "SELECT")
}

func whitelistRow() map[string]driver.Value {
	return map[string]driver.Value{"cfg_key": KeyIPWhitelist, "cfg_val": []byte(`["10.0.0.0/8"]`)}
}

// 命中缓存就不再查库：第二次读不该产生 SELECT。
func TestGetCachedServesFromMemory(t *testing.T) {
	f := gormfake.New()
	f.SetRow(whitelistRow())
	store := New(gormfake.MustOpen(f))

	raw, found, err := store.GetCached(KeyIPWhitelist)
	if err != nil || !found || string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("首次读应当回库拿到值：(%s, %v, %v)", raw, found, err)
	}
	after := selects(f)

	raw, found, err = store.GetCached(KeyIPWhitelist)
	if err != nil || !found || string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("第二次读应当回同样的值：(%s, %v, %v)", raw, found, err)
	}
	if got := selects(f); got != after {
		t.Fatalf("命中缓存不该再查库：SELECT 从 %d 涨到 %d", after, got)
	}
}

// 并发首访只查一次库：拿不到锁的那几个必须等锁、然后读到别人放进来的那份。
func TestGetCachedLoadsOnceConcurrently(t *testing.T) {
	f := gormfake.New()
	f.SetRow(whitelistRow())
	store := New(gormfake.MustOpen(f))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.GetCached(KeyIPWhitelist); err != nil {
				t.Errorf("GetCached: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := selects(f); got != 1 {
		t.Fatalf("20 个并发首访只该查 1 次库，实得 %d 次", got)
	}
}

// **行不存在**也是"加载过了"：下一次读不再查库（found=false 同样进缓存）。
func TestGetCachedRemembersMissingRow(t *testing.T) {
	f := gormfake.New() // 没有 SetRow = 一行都没有
	store := New(gormfake.MustOpen(f))

	if _, found, err := store.GetCached(KeyIPWhitelist); err != nil || found {
		t.Fatalf("没有这一行时应当是 (found=false, err=nil)，实得 (%v, %v)", found, err)
	}
	after := selects(f)
	if _, _, err := store.GetCached(KeyIPWhitelist); err != nil {
		t.Fatal(err)
	}
	if got := selects(f); got != after {
		t.Fatalf("「没配」也该被记住，不该反复查库：SELECT 从 %d 涨到 %d", after, got)
	}
}

// **读库失败不进缓存**：下一次请求要重试，而不是把"读不到"固化成一份空值。
func TestGetCachedDoesNotCacheErrors(t *testing.T) {
	f := gormfake.New()
	f.SetQueryError("crawler_config", "库抖了")
	store := New(gormfake.MustOpen(f))

	if _, _, err := store.GetCached(KeyIPWhitelist); err == nil {
		t.Fatal("查库出错应当冒出来")
	}

	// 库好了：这一次必须真的再查一遍（而不是命中上一次那份"读不到"）
	f2 := gormfake.New()
	f2.SetRow(whitelistRow())
	store2 := New(gormfake.MustOpen(f2))
	raw, found, err := store2.GetCached(KeyIPWhitelist)
	if err != nil || !found || string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("库恢复后应当读到值：(%s, %v, %v)", raw, found, err)
	}
}

// 键名不在注册表里：读也拦（与 Load 同一道门）—— 打错的键会静默多出一行没人读的配置。
func TestGetCachedRejectsUnknownKey(t *testing.T) {
	if _, _, err := New(gormfake.MustOpen(gormfake.New())).GetCached("ip_whitlist"); err == nil {
		t.Fatal("不在注册表里的键应当报错")
	}
}

// 写：先落库、再更新内存 —— 落库的语句出现过，紧接着的读命中缓存（不再查库）。
func TestSetCachedWritesThrough(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	if err := store.SetCached(KeyIPWhitelist, []string{"10.0.0.0/8"}, "ops"); err != nil {
		t.Fatalf("SetCached: %v", err)
	}
	if w := f.Written(); !strings.Contains(w, "cfg_val") {
		t.Fatalf("应当把这一行写进库：\n%s", w)
	}

	after := selects(f)
	raw, found, err := store.GetCached(KeyIPWhitelist)
	if err != nil || !found || string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("写完应当能读到自己刚写的值：(%s, %v, %v)", raw, found, err)
	}
	if got := selects(f); got != after {
		t.Fatalf("刚写完的键应当就在缓存里，不该再查库：%d → %d", after, got)
	}
}

// **库写失败不阻断内存更新**：err 返回给调用方记账，但缓存已经是新值 ——
// 这条是"暂停/恢复这类控制面动作不该因为投影写不进去就失效"的依据（见 engine/sitestat.go）。
func TestSetCachedKeepsMemoryOnDBFailure(t *testing.T) {
	f := gormfake.New()
	f.SetExecError("库抖了")
	store := New(gormfake.MustOpen(f))

	if err := store.SetCached(KeyIPWhitelist, []string{"10.0.0.0/8"}, "ops"); err == nil {
		t.Fatal("库写失败时应当把错误返回给调用方")
	}
	raw, found, err := store.GetCached(KeyIPWhitelist)
	if err != nil || !found {
		t.Fatalf("库写失败也不该影响内存那份：(%s, %v, %v)", raw, found, err)
	}
	if string(raw) != `["10.0.0.0/8"]` {
		t.Fatalf("内存里应当是新值，实得 %s", raw)
	}
}

// 站点快照：SetSites 落库 + 内存生效、读到的是按站点键排好序的切片。
func TestSitesRoundTrip(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))

	rows := []core.SiteStat{
		{Key: "b", BaseURL: "https://b.example/", StageCount: 2},
		{Key: "a", BaseURL: "https://a.example/", StageCount: 1, BreakerPaused: true},
	}
	if err := store.SetSites(rows); err != nil {
		t.Fatalf("SetSites: %v", err)
	}
	if w := f.Written(); !strings.Contains(w, "cfg_val") {
		t.Fatalf("站点快照要落进 crawler_config：\n%s", w)
	}

	got, err := store.Sites()
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" {
		t.Fatalf("应当按站点键排序：%+v", got)
	}
	if !got[0].BreakerPaused || got[1].BreakerPaused {
		t.Fatalf("暂停状态该原样带回来：%+v", got)
	}
}

// UpdateSites：**读-改-写在落点那层的写锁里**（cache.go 的规矩）。
// 并发改不同的站点时，两个改动都必须留下 —— 值是一整块 JSON，RMW 拆成两次加锁就会互相盖。
func TestUpdateSitesConcurrentKeepsEveryChange(t *testing.T) {
	f := gormfake.New()
	store := New(gormfake.MustOpen(f))
	if err := store.SetSites([]core.SiteStat{{Key: "a"}, {Key: "b"}, {Key: "c"}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, site := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			err := store.UpdateSites(func(rows []core.SiteStat) []core.SiteStat {
				for i := range rows {
					if rows[i].Key == key {
						rows[i].BreakerPaused = true
					}
				}
				return rows
			})
			if err != nil {
				t.Errorf("UpdateSites(%s): %v", key, err)
			}
		}(site)
	}
	wg.Wait()

	got, err := store.Sites()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("三个站都该在：%+v", got)
	}
	for _, r := range got {
		if !r.BreakerPaused {
			t.Fatalf("并发改的三个站必须都留下（别互相盖）：%+v", got)
		}
	}
}
