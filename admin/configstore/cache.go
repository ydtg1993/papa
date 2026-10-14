package configstore

import (
	"encoding/json"
)

// 本文件是 `crawler_config` 的**内存缓存层**：读走内存、写时先落库再更新内存。
//
// 为什么要它：这张表一行一键，而"读"可能很频繁（后台每 3 秒一刷的监控快照、引擎启动播种与
// 暂停/恢复），"写"极少 —— 每个请求查一次库是白费。
//
// 三条规矩（写调用方时照这三条来）：
//
//   - **读**：RLock 命中就直接回；没命中再用写锁查一次库（并发首访只查一次），放进缓存。
//     读库**失败不进缓存** —— 下一次请求会重试，别把"读不到"固化成一份空值。
//   - **写**：先 `Save`（落库）→ 再更新缓存；**库写失败不阻断缓存更新**，err 原样返回给调用方记账。
//     控制面动作（暂停/恢复）不该因为"给外部看的副本"写不进去就失效 —— 见 engine/sitestat.go 的取舍。
//   - **缓存里存 `cfg_val` 的原始字节**，不做类型解释；typed 读取口（如 `Sites`）各自解码。
//
// **每进程一份**：别的进程（另一个实例、运维直接改 SQL）写的值本进程看不见，直到下次 miss 或
// 显式 `Invalidate`。对 `sites` 这条数据无碍 —— 它的源在内存（声明 + 暂停信号），库那份是投影。
type cached struct {
	raw   []byte
	found bool
}

// GetCached 读一个键，优先走缓存。
func (s *Store) GetCached(key string) (raw []byte, found bool, err error) {
	if _, err := SpecOf(key); err != nil { // 键必须在注册表里（与 Load 同一道门）
		return nil, false, err
	}
	s.mu.RLock()
	if c, ok := s.cache[key]; ok {
		s.mu.RUnlock()
		return c.raw, c.found, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(key)
}

// SetCached 整块写一个键：先落库、再更新缓存（库写失败仍然更新缓存，并把 err 返回）。
func (s *Store) SetCached(key string, val any, operator string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setLocked(key, val, operator)
}

// Invalidate 让一个键的缓存失效（下次读回库里取）。给"外部改了库、想立刻看到"的调用方用。
func (s *Store) Invalidate(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, key)
}

// ---------- 以下都在 s.mu 的写锁里（调用方自己加锁） ----------

// getLocked 缓存里没有就读库并放进缓存。调用方必须持有 s.mu 写锁。
func (s *Store) getLocked(key string) (raw []byte, found bool, err error) {
	if c, ok := s.cache[key]; ok {
		return c.raw, c.found, nil
	}
	raw, found, err = s.Load(key)
	if err != nil {
		return nil, false, err
	}
	s.rememberLocked(key, raw, found)
	return raw, found, nil
}

// setLocked 先落库、再更新缓存（库写失败也更新），返回落库那个错误。
func (s *Store) setLocked(key string, val any, operator string) error {
	saveErr := s.Save(key, val, operator)
	raw, err := json.Marshal(val)
	if err == nil {
		s.rememberLocked(key, raw, true)
	}
	return saveErr
}

func (s *Store) rememberLocked(key string, raw []byte, found bool) {
	if s.cache == nil {
		s.cache = make(map[string]cached)
	}
	s.cache[key] = cached{raw: raw, found: found}
}
