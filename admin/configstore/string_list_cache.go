package configstore

import (
	"errors"
	"sync"
	"time"
)

// StringListReader 是 configstore 的一个**窄读口**：只负责把某个键读成字符串数组。
// 白名单这类"周期性重载、值很少变"的配置读它 —— 接口小到可以用装饰器随意包一层，
// 而不必把整个 *Store（写、建表、站点快照）拖进装饰器。
type StringListReader interface {
	LoadStrings(key string) ([]string, error)
}

// CachedStringListReader 是 StringListReader 的**装饰器**：在 TTL 内直接返回上次读到的值，
// 不查库；过期后查一次并刷新缓存。读失败**不缓存**（下一次调用会重试，别把一次抖动固化）。
//
// 为什么需要它：白名单由 `reloadWhitelistLoop` 每 30 秒重载一次（为了看见别的实例/运维直接改库），
// 于是每 30 秒一条 `SELECT ... WHERE cfg_key='ip_whitelist'`。值本身几乎不变，这条查询完全
// 可以按 TTL 摊薄。TTL 内的内存返回也保证重载循环不会因为一次读库失败把白名单清空。
//
// 一致性：本进程自己保存白名单时调用方要 `Invalidate`（storage 是唯一事实来源，不能拿旧缓存
// 覆盖刚保存的值）；其它进程/直接改 SQL 的变更，最迟在 TTL 后可见 —— 这是用"少查库"换来的
// 可见性延迟，写在常量注释里。
type CachedStringListReader struct {
	inner StringListReader
	ttl   time.Duration
	now   func() time.Time // 测试注入；生产为 time.Now

	mu      sync.Mutex
	entries map[string]stringListEntry
}

type stringListEntry struct {
	values   []string
	loadedAt time.Time
}

// NewCachedStringListReader 造一个带 TTL 的读装饰器。ttl <= 0 表示不缓存（直接透传）。
func NewCachedStringListReader(inner StringListReader, ttl time.Duration) *CachedStringListReader {
	return &CachedStringListReader{
		inner:   inner,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]stringListEntry),
	}
}

// LoadStrings 命中且未过期时返回缓存副本；否则回源读库并记住。
func (c *CachedStringListReader) LoadStrings(key string) ([]string, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("configstore: nil cached reader")
	}
	if c.ttl <= 0 {
		return c.inner.LoadStrings(key)
	}

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && c.now().Sub(entry.loadedAt) < c.ttl {
		values := append([]string(nil), entry.values...)
		c.mu.Unlock()
		return values, nil
	}
	c.mu.Unlock()

	values, err := c.inner.LoadStrings(key)
	if err != nil {
		return nil, err // 失败不写缓存
	}

	c.mu.Lock()
	c.entries[key] = stringListEntry{
		values:   append([]string(nil), values...),
		loadedAt: c.now(),
	}
	c.mu.Unlock()
	return append([]string(nil), values...), nil
}

// Invalidate 让一个键的缓存失效（本进程写入后调用，下次读回库里取新值）。
func (c *CachedStringListReader) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// InvalidateAll 让全部缓存失效（运维/测试需要强制回源时用）。
func (c *CachedStringListReader) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.entries = make(map[string]stringListEntry)
	c.mu.Unlock()
}
