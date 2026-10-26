package auth

import (
	"database/sql/driver"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

// countHits 统计假库收到的 COUNT 查询条数。
func countHits(f *gormfake.DB) int {
	return strings.Count(strings.ToLower(f.Written()), "count(")
}

func fixedTarget(f *gormfake.DB) (*gormfake.DB, *TokenCounter, *time.Time) {
	now := time.Now()
	counter := NewTokenCounter(gormfake.MustOpen(f), time.Minute)
	counter.now = func() time.Time { return now }
	return f, counter, &now
}

// TTL 内两种计数都只回源一次 —— 这正是“前端 3 秒轮询把 COUNT 刷进日志”的修法。
func TestTokenCounterCachesCountsWithinTTL(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 3)
	_, counter, _ := fixedTarget(f)

	for i := 0; i < 3; i++ {
		if n, err := counter.CountEnabled(); err != nil || n != 1 {
			t.Fatalf("第 %d 次 CountEnabled = (%d, %v)", i+1, n, err)
		}
	}
	for i := 0; i < 3; i++ {
		if n, err := counter.CountAll(); err != nil || n != 3 {
			t.Fatalf("第 %d 次 CountAll = (%d, %v)", i+1, n, err)
		}
	}
	if got := countHits(f); got != 2 {
		t.Fatalf("TTL 内查库次数 = %d，want 2（enabled 与 total 各一次）；实际 SQL：\n%s", got, f.Written())
	}
}

// 过期后必须回源：否则令牌全停用/新增都要等进程重启才被看见。
func TestTokenCounterRevalidatesAfterTTL(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	_, counter, now := fixedTarget(f)

	if n, _ := counter.CountEnabled(); n != 1 {
		t.Fatalf("首次 CountEnabled = %d", n)
	}
	*now = (*now).Add(time.Minute + time.Second)
	f.SetCounts(0, 0)

	if n, err := counter.CountEnabled(); err != nil || n != 0 {
		t.Fatalf("TTL 过期后 CountEnabled = (%d, %v)，want 0", n, err)
	}
	if n, err := counter.CountAll(); err != nil || n != 0 {
		t.Fatalf("TTL 过期后 CountAll = (%d, %v)，want 0", n, err)
	}
	if got := countHits(f); got != 3 {
		t.Fatalf("查库次数 = %d，want 3（首次 1 + 过期后 2）；实际 SQL：\n%s", got, f.Written())
	}
}

// 本进程写完令牌表后 Invalidate：下一次读必须回源，不能拿旧值把刚写的结果盖住。
func TestTokenCounterInvalidateForcesReload(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	_, counter, _ := fixedTarget(f)

	if n, _ := counter.CountEnabled(); n != 1 {
		t.Fatalf("首次 CountEnabled = %d", n)
	}
	if n, _ := counter.CountAll(); n != 1 {
		t.Fatalf("首次 CountAll = %d", n)
	}
	f.SetCounts(0, 0)
	counter.Invalidate()

	if n, err := counter.CountEnabled(); err != nil || n != 0 {
		t.Fatalf("Invalidate 后 CountEnabled = (%d, %v)，want 0", n, err)
	}
	if n, err := counter.CountAll(); err != nil || n != 0 {
		t.Fatalf("Invalidate 后 CountAll = (%d, %v)，want 0", n, err)
	}
	if got := countHits(f); got != 4 {
		t.Fatalf("查库次数 = %d，want 4（首次 2 + 失效后 2）；实际 SQL：\n%s", got, f.Written())
	}
}

// 读失败不缓存：一次库抖动不能被固化成“0 条令牌”，否则后台会悄悄变成开放。
func TestTokenCounterDoesNotCacheErrors(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	_, counter, _ := fixedTarget(f)

	f.SetQueryError("count(", "db down")
	if _, err := counter.CountEnabled(); err == nil {
		t.Fatal("查库失败应当返回错误")
	}

	f.SetQueryError("", "")
	if n, err := counter.CountEnabled(); err != nil || n != 1 {
		t.Fatalf("恢复后 CountEnabled = (%d, %v)，want 1", n, err)
	}
	// 恢复后的成功结果要进缓存：再读一次不该新增查询。
	before := countHits(f)
	if n, _ := counter.CountEnabled(); n != 1 {
		t.Fatalf("第二次成功读取 = %d", n)
	}
	if got := countHits(f) - before; got != 0 {
		t.Fatalf("成功后又被缓存了却仍查库 %d 次；实际 SQL：\n%s", got, f.Written())
	}
}

// ttl<=0 等于不缓存（旧语义透传），方便调用方显式关掉。
func TestTokenCounterZeroTTLPassthrough(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(2, 5)
	counter := NewTokenCounter(gormfake.MustOpen(f), 0)

	for i := 0; i < 2; i++ {
		if _, err := counter.CountEnabled(); err != nil {
			t.Fatal(err)
		}
		if _, err := counter.CountAll(); err != nil {
			t.Fatal(err)
		}
	}
	if got := countHits(f); got != 4 {
		t.Fatalf("ttl=0 时查库次数 = %d，want 4", got)
	}
}

// 缓存的是“是否配过凭据”，不是令牌本身：具体令牌仍逐请求查库。
func TestTokenCounterStillVerifiesEachToken(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	f.SetRow(map[string]driver.Value{"operator": "alice"})
	counter := NewTokenCounter(gormfake.MustOpen(f), time.Minute)

	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("GET", "/api/monitor", nil)
		r.Header.Set("Authorization", "Bearer good-token")
		op, ok := counter.Verifier(nil)(r)
		if !ok || op != "alice" {
			t.Fatalf("第 %d 次校验 = (%q, %v)", i+1, op, ok)
		}
	}
	if got := countHits(f); got != 1 {
		t.Fatalf("令牌数量查库次数 = %d，want 1；实际 SQL：\n%s", got, f.Written())
	}
	if got := strings.Count(strings.ToLower(f.Written()), "token_hash"); got != 3 {
		t.Fatalf("令牌校验次数 = %d，want 3（每个请求都要查）；实际 SQL：\n%s", got, f.Written())
	}
}

// Verifier 的“表为空 → 放行”分支也要吃缓存：这是用户日志里 COUNT 刷屏的源头。
func TestTokenCounterVerifierUsesCachedCounts(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(0, 0)
	_, counter, _ := fixedTarget(f)

	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("GET", "/api/monitor", nil)
		if _, ok := counter.Verifier(nil)(r); !ok {
			t.Fatalf("第 %d 次：一条令牌都没有时应放行", i+1)
		}
	}
	if got := countHits(f); got != 2 {
		t.Fatalf("5 次校验共查库 %d 次，want 2（enabled + total 各一次）；实际 SQL：\n%s", got, f.Written())
	}
}

// nil db / nil 接收者按“没配凭据”处理，不 panic。
func TestTokenCounterNilDatabase(t *testing.T) {
	counter := NewTokenCounter(nil, time.Minute)
	if n, err := counter.CountEnabled(); n != 0 || err != nil {
		t.Fatalf("nil db CountEnabled = (%d, %v)", n, err)
	}
	if n, err := counter.CountAll(); n != 0 || err != nil {
		t.Fatalf("nil db CountAll = (%d, %v)", n, err)
	}
	if counter.Required() {
		t.Fatal("nil db 时 Required 应为 false")
	}
	r := httptest.NewRequest("GET", "/api/monitor", nil)
	if _, ok := counter.Verifier(nil)(r); !ok {
		t.Fatal("nil db 时 Verifier 应放行（未配置凭据）")
	}
	if _, ok := counter.Confirmer(nil)("whatever"); !ok {
		t.Fatal("nil db 时 Confirmer 应放行（未配置凭据）")
	}

	var nilCounter *TokenCounter
	nilCounter.Invalidate() // 不该 panic
	if n, err := nilCounter.CountEnabled(); n != 0 || err != nil {
		t.Fatalf("nil 接收者 CountEnabled = (%d, %v)", n, err)
	}
}
