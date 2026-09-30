package crawler

import (
	"container/list"
	"sync"
)

// dedupCache 有界 LRU 去重缓存。key 为任务去重键（Task.Unique()），value 恒为占位。
// 容量 <= 0 表示不限制（保持旧行为）；>0 时超出容量淘汰最久未使用的条目，
// 被淘汰条目的去重由 DB 唯一索引（idx_stage_url）兜底。
type dedupCache struct {
	mu      sync.Mutex
	cap     int
	entries map[string]*list.Element
	lru     *list.List
}

type dedupEntry struct {
	key string
}

func newDedupCache(capacity int) *dedupCache {
	return &dedupCache{
		cap:     capacity,
		entries: make(map[string]*list.Element),
		lru:     list.New(),
	}
}

// Get 返回 key 是否存在，命中则刷新其 LRU 位置。
func (c *dedupCache) Get(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return false
	}
	c.lru.MoveToFront(el)
	return true
}

// Add 插入或刷新 key；超容量时淘汰最久未使用的条目。
func (c *dedupCache) Add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.lru.MoveToFront(el)
		return
	}
	el := c.lru.PushFront(&dedupEntry{key: key})
	c.entries[key] = el
	if c.cap > 0 && c.lru.Len() > c.cap {
		c.evictOldestLocked()
	}
}

// Delete 删除指定 key。
func (c *dedupCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.lru.Remove(el)
		delete(c.entries, key)
	}
}

// Len 返回当前条目数。
func (c *dedupCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

func (c *dedupCache) evictOldestLocked() {
	el := c.lru.Back()
	if el == nil {
		return
	}
	e := el.Value.(*dedupEntry)
	c.lru.Remove(el)
	delete(c.entries, e.key)
}
