package engine

import (
	"github.com/ydtg1993/papa/v3/internal/track"
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
	base["error_retry_total"] = e.errorRetriedCount.Load()
	base["repeat_repoll_total"] = e.repeatRepolledCount.Load()
	return base
}

// setStatsQueue 设置阶段统计信息管理器
func (e *Engine) setStatsQueue(stage string, mon *track.StatsQueue[*Task]) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.statsQueue == nil {
		e.statsQueue = make(map[string]*track.StatsQueue[*Task])
	}
	e.statsQueue[stage] = mon
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
			Global:  GlobalStats(mon.GetGlobalStats()),
			Workers: workers,
			Queue: QueueStats{
				Submitted:  submitted,
				Completed:  completed,
				Failed:     failed,
				InProgress: inProgress,
				QueueLen:   queueLen,
			},
		}
	}
	return out
}
