package metrics

import (
	"fmt"
	"sync"
	"testing"
)

func TestRegistrySetOverwriteAndSnapshot(t *testing.T) {
	r := New()
	if got := r.GetAll(); len(got) != 0 {
		t.Fatalf("新注册表应为空，实得 %v", got)
	}

	r.Set("a", 1)
	r.Set("b", "x")
	r.Set("a", 2) // 同 key 覆盖

	got := r.GetAll()
	if got["a"] != 2 || got["b"] != "x" {
		t.Fatalf("GetAll = %v, want a=2 b=x", got)
	}
}

// GetAll 是**浅拷贝**：删掉/改写快照不能反过来动到注册表本身。
// 监控页每轮刷新都会调它，共用一个 map 迟早会被写坏。
func TestGetAllReturnsIndependentMap(t *testing.T) {
	r := New()
	r.Set("a", 1)
	r.Set("b", 2)

	snap := r.GetAll()
	snap["a"] = 999
	delete(snap, "b")
	snap["new"] = 3

	again := r.GetAll()
	if again["a"] != 1 {
		t.Fatalf("改快照影响了注册表：a = %v, want 1", again["a"])
	}
	if again["b"] != 2 {
		t.Fatalf("删快照影响了注册表：b = %v, want 2", again["b"])
	}
	if _, ok := again["new"]; ok {
		t.Fatalf("往快照里加 key 影响了注册表：%v", again)
	}
}

// 写入方（fetcher）与读取方（监控页）是两条并发路径，必须经得起 -race。
func TestRegistryConcurrentAccess(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				r.Set(fmt.Sprintf("k%d", j%10), n)
				_ = r.GetAll()
			}
		}(i)
	}
	wg.Wait()

	if got := len(r.GetAll()); got != 10 {
		t.Fatalf("并发写之后应有 10 个 key，实得 %d", got)
	}
}
