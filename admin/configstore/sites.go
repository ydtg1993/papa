package configstore

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ydtg1993/papa/v3/core"
)

// 站点快照（`cfg_key = sites`）：**内存是源，库是投影**。
//
// 为什么站点放进配置表、而不是自己一张表：这份数据只有两类 —— ①声明里抄来的（Key / BaseURL /
// 阶段数）②运行期的暂停状态（后台点「暂停整站」/「恢复整站」）。两张表加起来只服务一个**只读**的
// 后台页面，却要付一份模型、一次迁移、两条写路径。放进这里之后：模型与迁移没了，写路径只剩
// "整块替换一次"。
//
// 语义（重要，别按普通配置去理解它）：
//   - **引擎启动时按声明重建**（`engine.buildSiteStats`），不读回旧值 —— 暂停状态
//     **不跨重启**，读回旧值等于把上次的暂停态带回来（那是行为回退）。
//   - **写是整块替换**，不是"改其中一项"：站点列表的源在声明与引擎内存里，
//     这里只是一个投影。所以并发安全靠"同一把写锁里读-改-写"（`UpdateSites`），不靠行级条件更新。
//   - 多实例下**最后写入的为准**（整块 blob），库那份不构成跨实例的共识 —— 跨实例的真相各自在内存里。
const KeySites = "sites"

// sitesOf 解码缓存里的站点数组；行不存在或空值 = 还没有快照（返回 nil，不是错误）。
func (s *Store) sitesOfLocked() ([]core.SiteStat, error) {
	raw, found, err := s.getLocked(KeySites)
	if err != nil {
		return nil, err
	}
	return decodeSites(raw, found)
}

// Sites 读站点快照（内存优先）。返回切片按 Key 排序（调用方与页面都省一次排序）。
func (s *Store) Sites() ([]core.SiteStat, error) {
	// 快路径：命中缓存只拿读锁 —— 监控页每 3 秒一刷，别让它们互相排队
	s.mu.RLock()
	if c, ok := s.cache[KeySites]; ok {
		raw, found := c.raw, c.found
		s.mu.RUnlock()
		return decodeSites(raw, found)
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.sitesOfLocked()
	if err != nil {
		return nil, err
	}
	sortSiteStats(rows)
	return rows, nil
}

// decodeSites 解码一份站点数组（行不存在/空值 = 还没有快照，返回 nil 而不是错误）。
func decodeSites(raw []byte, found bool) ([]core.SiteStat, error) {
	if !found || len(raw) == 0 {
		return nil, nil
	}
	var out []core.SiteStat
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("配置 %s 的值不是站点数组: %w", KeySites, err)
	}
	sortSiteStats(out)
	return out, nil
}

// SetSites 整块写站点快照（引擎启动播种用）：先落库、再更新内存，库写失败不阻断内存。
func (s *Store) SetSites(rows []core.SiteStat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setLocked(KeySites, rows, siteWriter)
}

// UpdateSites 在**同一把写锁里**读-改-写整块快照：暂停/恢复走这条。
//
// 为什么必须由这一层来读-改-写：值是一整块 JSON，两次并发的"读出来→改一站→写回去"
// 会互相盖掉（后写的把前一次改的另一个站抹平）。把 RMW 放进同一把锁里就没有这个窗口。
// mutate 拿到的切片可以原地改（它是从缓存解码出来的副本）。
func (s *Store) UpdateSites(mutate func(rows []core.SiteStat) []core.SiteStat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.sitesOfLocked()
	if err != nil {
		return err
	}
	return s.setLocked(KeySites, mutate(rows), siteWriter)
}

// siteWriter 这一行最后被谁改的：站点快照是**引擎**写的（不是某个操作人 —— 谁点的记在
// crawler_operation_log 里，那一份才是审计）。
const siteWriter = "engine"

// sortSiteStats 按站点键排序（就地）。
func sortSiteStats(rows []core.SiteStat) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
}
