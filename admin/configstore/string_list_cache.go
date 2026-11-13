package configstore

import (
	"context"
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

// ContextStringListReader 把生命周期取消传到读操作；实现者应把 ctx 传到 SQL。
type ContextStringListReader interface {
	LoadStringsContext(ctx context.Context, key string) ([]string, error)
}

// CachedStringListReader 是 StringListReader 的**装饰器**：在 TTL 内直接返回上次读到的值，
// 不查库；过期后查一次并刷新缓存。读失败**不缓存**（下一次调用会重试，别把一次抖动固化）。
//
// 为什么需要它：白名单每 30 秒重载一次（为了看见运维直接改库），
// 于是每 30 秒一条 `SELECT ... WHERE cfg_key='ip_whitelist'`。值本身几乎不变，这条查询完全
// 可以按 TTL 摊薄。TTL 内的内存返回也保证重载循环不会因为一次读库失败把白名单清空。
//
// 一致性：本进程自己保存白名单时调用方要 `Invalidate`（storage 是唯一事实来源，不能拿旧缓存
// 覆盖刚保存的值）；其它进程/直接改 SQL 的变更，最迟在 TTL 后可见 —— 这是用"少查库"换来的
// 可见性延迟，写在常量注释里。
type CachedStringListReader struct {
	inner ContextStringListReader
	ttl   time.Duration
	now   func() time.Time // 测试注入；生产为 time.Now

	mu      sync.Mutex
	entries map[string]stringListEntry
	version uint64 // 失效期间完成的旧读取不能回填缓存或返回给发布者。
}

type stringListEntry struct {
	values   []string
	loadedAt time.Time
}

// NewCachedStringListReader 造一个带 TTL 的读装饰器。ttl <= 0 表示不缓存（直接透传）。
func NewCachedStringListReader(inner StringListReader, ttl time.Duration) *CachedStringListReader {
	if contextual, ok := inner.(ContextStringListReader); ok {
		return NewCachedContextStringListReader(contextual, ttl)
	}
	if inner == nil {
		return NewCachedContextStringListReader(nil, ttl)
	}
	return NewCachedContextStringListReader(legacyStringListReader{inner}, ttl)
}

// NewCachedContextStringListReader 装饰带 context 的窄读口。
func NewCachedContextStringListReader(inner ContextStringListReader, ttl time.Duration) *CachedStringListReader {
	return &CachedStringListReader{
		inner:   inner,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]stringListEntry),
	}
}

type legacyStringListReader struct{ StringListReader }

func (r legacyStringListReader) LoadStringsContext(ctx context.Context, key string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 旧接口不能中断已经开始的读；宿主仍须等待真正返回后才能关闭资源。
	values, err := r.LoadStrings(key)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return values, err
}

// LoadStrings 命中且未过期时返回缓存副本；否则回源读库并记住。
func (c *CachedStringListReader) LoadStrings(key string) ([]string, error) {
	return c.LoadStringsContext(context.Background(), key)
}

// LoadStringsContext 保留 TTL 语义；取消和读失败都不写缓存。
func (c *CachedStringListReader) LoadStringsContext(ctx context.Context, key string) ([]string, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("configstore: nil cached reader")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		if entry, ok := c.entries[key]; ok && c.ttl > 0 && c.now().Sub(entry.loadedAt) < c.ttl {
			values := append([]string(nil), entry.values...)
			c.mu.Unlock()
			return values, nil
		}
		version := c.version
		c.mu.Unlock()

		values, err := c.inner.LoadStringsContext(ctx, key)
		c.mu.Lock()
		if ctx.Err() != nil {
			c.mu.Unlock()
			return nil, ctx.Err()
		}
		if version != c.version {
			c.mu.Unlock()
			continue // 读取期间发生写后失效；重新读，不能发布写入前的旧值。
		}
		if err != nil {
			c.mu.Unlock()
			return nil, err // 失败不写缓存
		}
		if c.ttl > 0 {
			c.entries[key] = stringListEntry{
				values:   append([]string(nil), values...),
				loadedAt: c.now(),
			}
		}
		c.mu.Unlock()
		return append([]string(nil), values...), nil
	}
}

// Invalidate 让一个键的缓存失效（本进程写入后调用，下次读回库里取新值）。
func (c *CachedStringListReader) Invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.version++
	delete(c.entries, key)
	c.mu.Unlock()
}

// InvalidateAll 让全部缓存失效（运维/测试需要强制回源时用）。
func (c *CachedStringListReader) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.version++
	c.entries = make(map[string]stringListEntry)
	c.mu.Unlock()
}
