package engine

import (
	"container/list"
	"sync"

	"github.com/ydtg1993/papa/v2/models"
)

// dedupCache 有界 LRU 去重缓存。key 为任务去重键（Task.Unique()），value 恒为占位。
// 容量 <= 0 表示不限制（保持旧行为）；>0 时超出容量淘汰最久未使用的条目，
// 被淘汰条目的去重由 DB 唯一索引（idx_stage_url_hash）兜底。
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

// DelActiveTask 从去重任务列表中删除任务
func (e *Engine) DelActiveTask(task *Task) {
	e.dedupCache.Delete(task.Unique())
}

// loadActiveTasks 启动时把「进行中」任务（pending/processing）与全部轮询任务加载进内存去重表，
// 避免全表加载导致内存随历史任务无限增长；已完成任务由 DB 唯一索引兜底去重。
func (e *Engine) loadActiveTasks() {
	var tasks []models.CrawlerTask
	err := e.db.Where("status IN ? OR repeatable = ?",
		[]models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing},
		models.RepeatableYes).
		Find(&tasks).Error
	if err != nil {
		e.loggerSet.DB.Errorf("load active tasks failed: %s", err.Error())
		return
	}
	for _, t := range tasks {
		task := Task{URL: t.URL, Stage: t.Stage, IdempotencyKey: t.IdempotencyKey}
		e.dedupCache.Add(task.Unique())
	}
}
