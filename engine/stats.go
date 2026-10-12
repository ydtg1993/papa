package engine

import (
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// RecordMetric 写入一条业务自定义监控数据，供监控页展示
func (e *Engine) RecordMetric(key string, v any) {
	if e.metrics == nil {
		return
	}
	e.metrics.Set(key, v)
}

// GetMetrics 返回监控数据快照：业务自定义数据 + 框架级队列治理计数（溢出/恢复/失败重投）。
func (e *Engine) GetMetrics() map[string]any {
	base := map[string]any{}
	if e.metrics != nil {
		base = e.metrics.GetAll()
	}
	base["queue_spilled"] = e.spilledCount.Load()
	base["queue_spill_backlog"] = e.spillBacklog()
	base["recover_total"] = e.recoveredCount.Load()
	base["error_retry_total"] = e.errorRetriedTotal()
	base["repeat_repoll_total"] = e.repeatRepolledTotal()
	return base
}

// setStatsQueue 设置阶段统计信息管理器
func (e *Engine) setStatsQueue(stage string, mon *track.StatsQueue[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.statsQueue == nil {
		e.statsQueue = make(map[string]*track.StatsQueue[*Task])
	}
	// 池被懒建/重建时会换一份新的 stats：先停掉上一份的 goroutine，别让它泄漏到引擎结束。
	if old, ok := e.statsQueue[stage]; ok {
		old.Stop()
	}
	e.statsQueue[stage] = mon
}

// stageTotals 一个阶段的累计吞吐（池 churn 不清零）：只累计三个计数，worker 时长统计仍是当前实例。
type stageTotals struct {
	Submitted int64
	Completed int64
	Failed    int64
}

// harvestStageTotals 把一份被回收池的最终吞吐计数累进该阶段的累计值。
// 按具体的那份 pool 走（与 removeStageStats 的身份校验同一思路）：回收与收割之间池被重建，
// 累进的仍是这份被回收池的计数，不会错加到新池头上。
func (e *Engine) harvestStageTotals(stage string, pool *workerpool.WorkerPool[*Task]) {
	submitted, completed, failed, _, _ := pool.Stats()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stageTotals == nil {
		e.stageTotals = make(map[string]stageTotals)
	}
	t := e.stageTotals[stage]
	t.Submitted += submitted
	t.Completed += completed
	t.Failed += failed
	e.stageTotals[stage] = t
}

// GetStageStats 返回各阶段统计快照（纯值类型，供监控页读取，不暴露内部实现）
func (e *Engine) GetStageStats() map[string]StageStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]StageStats, len(e.statsQueue))
	for stage, mon := range e.statsQueue {
		submitted, completed, failed, inProgress, queueLen := mon.WorkPool.Stats()
		allWorkers := mon.GetAllWorkerStats()
		workers := make(map[int]WorkerStat, len(allWorkers))
		for id, w := range allWorkers {
			workers[id] = WorkerStat(w)
		}
		out[stage] = StageStats{
			Site:    e.siteOf(stage), // 阶段属于哪个站点（后台按站点分组就靠它）
			Global:  GlobalStats(mon.GetGlobalStats()),
			Workers: workers,
			Queue: QueueStats{
				// 累计（跨池实例） + 当前池：池 churn 后吞吐不归零
				Submitted:  e.stageTotals[stage].Submitted + submitted,
				Completed:  e.stageTotals[stage].Completed + completed,
				Failed:     e.stageTotals[stage].Failed + failed,
				InProgress: inProgress,
				QueueLen:   queueLen,
			},
		}
	}
	return out
}
