package engine

import "testing"

func TestDedupCacheUnbounded(t *testing.T) {
	c := newDedupCache(0)
	c.Add("a")
	c.Add("b")
	if !c.Get("a") || !c.Get("b") {
		t.Fatal("expected both keys present when unbounded")
	}
	c.Delete("a")
	if c.Get("a") {
		t.Fatal("expected key a deleted")
	}
	if !c.Get("b") {
		t.Fatal("expected key b still present")
	}
}

func TestDedupCacheLRUEviction(t *testing.T) {
	c := newDedupCache(2)
	c.Add("a")
	c.Add("b")
	c.Add("c") // 淘汰最久未使用的 a
	if c.Get("a") {
		t.Fatal("expected a evicted")
	}
	if !c.Get("b") || !c.Get("c") {
		t.Fatal("expected b and c present")
	}
}

func TestDedupCacheGetRefreshesLRU(t *testing.T) {
	c := newDedupCache(2)
	c.Add("a")
	c.Add("b")
	c.Get("a") // 刷新 a
	c.Add("c") // 应淘汰 b
	if c.Get("b") {
		t.Fatal("expected b evicted")
	}
	if !c.Get("a") || !c.Get("c") {
		t.Fatal("expected a and c present")
	}
}

func TestDedupCacheLen(t *testing.T) {
	c := newDedupCache(3)
	c.Add("a")
	c.Add("b")
	if c.Len() != 2 {
		t.Fatalf("expected len 2, got %d", c.Len())
	}
	c.Add("a") // 重复 add 不增长
	if c.Len() != 2 {
		t.Fatalf("expected len 2 after duplicate add, got %d", c.Len())
	}
}
