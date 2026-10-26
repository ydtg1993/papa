package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/admin/tokenadmin"
	"github.com/ydtg1993/papa/v3/internal/gormfake"
)

func countHits(f *gormfake.DB) int {
	return strings.Count(strings.ToLower(f.Written()), "count(")
}

// 令牌页写成功后必须让数量缓存失效：否则新建第一把令牌后后台仍会按“还没配凭据”
// 开放到 TTL 到期，登录遮罩也不会出现。
func TestTokenWriteHookInvalidatesCountCache(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	counter := auth.NewTokenCounter(gormfake.MustOpen(f), time.Minute)

	if n, err := counter.CountEnabled(); err != nil || n != 1 {
		t.Fatalf("首次 CountEnabled = (%d, %v)", n, err)
	}
	if got := countHits(f); got != 1 {
		t.Fatalf("首次查库次数 = %d，want 1", got)
	}

	f.SetCounts(0, 0)
	tokenWriteHook(counter, nil)(tokenadmin.Event{Action: "create"})

	if n, err := counter.CountEnabled(); err != nil || n != 0 {
		t.Fatalf("失效后 CountEnabled = (%d, %v)，want 0", n, err)
	}
	if got := countHits(f); got != 2 {
		t.Fatalf("失效后查库次数 = %d，want 2；实际 SQL：\n%s", got, f.Written())
	}
}

// 写失败不失效：数据没变，失效只会让每个请求多查一次库。
func TestTokenWriteHookKeepsCacheOnFailure(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(1, 1)
	counter := auth.NewTokenCounter(gormfake.MustOpen(f), time.Minute)
	if n, err := counter.CountEnabled(); err != nil || n != 1 {
		t.Fatalf("首次 CountEnabled = (%d, %v)", n, err)
	}

	tokenWriteHook(counter, nil)(tokenadmin.Event{Action: "create", Err: errors.New("db down")})

	if n, err := counter.CountEnabled(); err != nil || n != 1 {
		t.Fatalf("失败后 CountEnabled = (%d, %v)，want 仍为缓存值 1", n, err)
	}
	if got := countHits(f); got != 1 {
		t.Fatalf("失败事件不该失效缓存，查库次数 = %d，want 1", got)
	}
}
