package engine

import (
	"container/list"
	"sync"

	"github.com/ydtg1993/papa/v4/models"
)

// dedupCache 有界 LRU 去重缓存。key 为任务去重键（Task.Unique()），value 恒为占位。
// 容量 <= 0 表示不限制（保持旧行为）；>0 时超出容量淘汰最久未使用的条目，
// 被淘汰条目的去重由 DB 唯一索引（idx_site_stage_url_hash）兜底。
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

// loadActiveTasks 分页预热 pending/processing 与轮询任务；每页最多 256 行。
// 有限缓存只读容量内最近 updated_at/id 的记录；0 保持无限缓存，但查询暂存按页有界。
// 无限模式按 id 游标遍历，避免大表 OFFSET 累积扫描；有限模式 OFFSET 最多到容量。
// 只投影去重键与游标列，不加载 error/meta 等宽列。未预热的记录由 DB 唯一索引兜底。
func (e *Engine) loadActiveTasks() {
	const pageSize = 256
	capacity, loaded := e.dedupCache.cap, 0
	var lastID uint
	for {
		limit := pageSize
		if capacity > 0 {
			limit = min(limit, capacity-loaded)
			if limit <= 0 {
				return
			}
		}
		query := e.db.Select("id", "url", "stage", "site", "idempotency_key").
			Where("status IN ? OR repeatable = ?",
				[]models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}, models.RepeatableYes)
		if capacity > 0 {
			query = query.Order("updated_at DESC").Order("id DESC").Offset(loaded)
		} else {
			query = query.Order("id DESC")
			if loaded > 0 {
				query = query.Where("id < ?", lastID)
			}
		}
		var tasks []models.CrawlerTask
		if err := query.Limit(limit).Find(&tasks).Error; err != nil {
			e.loggerSet.DB.Errorf("load active tasks failed: %s", err.Error())
			return
		}
		for _, t := range tasks {
			// Site 和自定义幂等键必须保留，与实际投递使用同一种任务身份。
			task := Task{URL: t.URL, Stage: t.Stage, Site: t.Site, IdempotencyKey: t.IdempotencyKey}
			e.dedupCache.addOlder(task.Unique())
		}
		loaded += len(tasks)
		if len(tasks) < limit {
			return
		}
		lastID = tasks[len(tasks)-1].ID
	}
}

// addOlder 按查询的新到旧顺序追加，保持预热的 LRU 顺序；不覆盖已在缓存中的更新记录。
func (c *dedupCache) addOlder(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok || (c.cap > 0 && c.lru.Len() >= c.cap) {
		return
	}
	c.entries[key] = c.lru.PushBack(&dedupEntry{key: key})
}
