package engine

import (
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// setStatsQueue 设置阶段统计信息管理器（key 是 `core.StageKey`：两个站的同名阶段各有各的统计）
func (e *Engine) setStatsQueue(key string, mon *track.StatsQueue[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.statsQueue == nil {
		e.statsQueue = make(map[string]*track.StatsQueue[*Task])
	}
	// 池被懒建/重建时会换一份新的 stats：先停掉上一份的 goroutine，别让它泄漏到引擎结束。
	if old, ok := e.statsQueue[key]; ok {
		old.Stop()
	}
	e.statsQueue[key] = mon
}

// stageTotals 一个阶段的累计吞吐（池 churn 不清零）：只累计三个计数，worker 时长统计仍是当前实例。
type stageTotals struct {
	Submitted int64
	Completed int64
	Failed    int64
}

// harvestStageTotals 把一份被回收池的最终吞吐计数累进该阶段（key = `core.StageKey`）的累计值。
// 按具体的那份 pool 走（与 removeStageStats 的身份校验同一思路）：回收与收割之间池被重建，
// 累进的仍是这份被回收池的计数，不会错加到新池头上。
func (e *Engine) harvestStageTotals(key string, pool *workerpool.WorkerPool[*Task]) {
	submitted, completed, failed, _, _ := pool.Stats()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stageTotals == nil {
		e.stageTotals = make(map[string]stageTotals)
	}
	t := e.stageTotals[key]
	t.Submitted += submitted
	t.Completed += completed
	t.Failed += failed
	e.stageTotals[key] = t
}

// GetStageStats 返回各阶段统计快照（纯值类型，供监控页读取，不暴露内部实现）。
//
// **map 的键是 `core.StageKey(site, stage)`**（阶段名只在本站内唯一，两个站的 catalog 是两条）；
// 站点与裸阶段名另各带一个字段，后台在站点 Tab 里显示短名字、按站分组都靠它们。
func (e *Engine) GetStageStats() map[string]StageStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]StageStats, len(e.statsQueue))
	for key, mon := range e.statsQueue {
		// 键没在登记表里 = 只有统计器没有阶段（测试会这么塞）。两段留空，不 panic：
		// 监控页少一行标注，好过整个 API 500。
		var stage, site string
		if info := e.stages[key]; info != nil {
			stage, site = info.stage, info.config.Site
		}
		submitted, completed, failed, inProgress, queueLen := mon.WorkPool.Stats()
		allWorkers := mon.GetAllWorkerStats()
		workers := make(map[int]WorkerStat, len(allWorkers))
		for id, w := range allWorkers {
			workers[id] = WorkerStat(w)
		}
		out[key] = StageStats{
			Stage:   stage,
			Site:    site, // 阶段属于哪个站点（后台按站点分组就靠它）
			Global:  GlobalStats(mon.GetGlobalStats()),
			Workers: workers,
			Queue: QueueStats{
				// 累计（跨池实例） + 当前池：池 churn 后吞吐不归零
				Submitted:  e.stageTotals[key].Submitted + submitted,
				Completed:  e.stageTotals[key].Completed + completed,
				Failed:     e.stageTotals[key].Failed + failed,
				InProgress: inProgress,
				QueueLen:   queueLen,
			},
		}
	}
	return out
}
