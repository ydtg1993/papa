package monitor

import (
	"log"
	"papa/pkg/workerpool"
	"sync"
	"time"
)

// WorkerStat 记录单个 worker 的统计信息
type WorkerStat struct {
	WorkerID    int
	TotalTasks  int64         // 已处理任务总数
	FailedTasks int64         // 失败任务数
	TotalTime   time.Duration // 总耗时（所有任务耗时之和）
	MaxTime     time.Duration // 最长单个任务耗时
	MinTime     time.Duration // 最短单个任务耗时
}

// GlobalStats 全局统计
type GlobalStats struct {
	TotalTasks  int64         // 总处理任务数
	TotalFailed int64         // 总失败任务数
	TotalTime   time.Duration // 所有任务总耗时
	AvgTime     time.Duration // 平均任务耗时
	MaxTime     time.Duration // 最长任务耗时
	MinTime     time.Duration // 最短任务耗时
}

// Monitor 监控消费者，统计任务和 worker 的活动
type Monitor[T workerpool.Tasker] struct {
	WorkPool    *workerpool.WorkerPool[T]
	mu          sync.RWMutex
	workerStats map[int]*WorkerStat // key: workerID
	globalStats GlobalStats
	stopCh      chan struct{}
}

// NewMonitor 创建监控器
func NewMonitor[T workerpool.Tasker](pool *workerpool.WorkerPool[T]) *Monitor[T] {
	return &Monitor[T]{
		WorkPool:    pool,
		workerStats: make(map[int]*WorkerStat),
		stopCh:      make(chan struct{}),
	}
}

// Start 启动监控（在一个 goroutine 中）
func (m *Monitor[T]) Start() {
	go m.run()
}

// run 持续消费 Activities 通道
func (m *Monitor[T]) run() {
	activities := m.WorkPool.Activities()
	for {
		select {
		case <-m.stopCh:
			return
		case act, ok := <-activities:
			if !ok {
				// 通道已关闭，退出
				log.Println("monitor: activities channel closed, stopping")
				return
			}
			m.processActivity(act)
		}
	}
}

// processActivity 处理单个活动事件
func (m *Monitor[T]) processActivity(act workerpool.Activity) {
	switch act.Type {
	case workerpool.ActivityTaskStart:
		// 任务开始：可以记录开始时间到某个临时存储，但这里我们不记录，因为结束时已有 Duration
		// 如果需要在开始事件中做统计，可以预留
	case workerpool.ActivityTaskEnd:
		m.updateStats(act)
	}
}

// updateStats 更新统计信息
func (m *Monitor[T]) updateStats(act workerpool.Activity) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 获取或创建 worker 统计
	ws, ok := m.workerStats[act.WorkerID]
	if !ok {
		ws = &WorkerStat{
			WorkerID: act.WorkerID,
			MinTime:  act.Duration,
		}
		m.workerStats[act.WorkerID] = ws
	}

	// 更新 worker 统计
	ws.TotalTasks++
	if act.Error != nil {
		ws.FailedTasks++
	}
	ws.TotalTime += act.Duration
	if act.Duration > ws.MaxTime {
		ws.MaxTime = act.Duration
	}
	if act.Duration < ws.MinTime {
		ws.MinTime = act.Duration
	}

	// 更新全局统计
	m.globalStats.TotalTasks++
	if act.Error != nil {
		m.globalStats.TotalFailed++
	}
	m.globalStats.TotalTime += act.Duration
	if act.Duration > m.globalStats.MaxTime {
		m.globalStats.MaxTime = act.Duration
	}
	if act.Duration < m.globalStats.MinTime || m.globalStats.MinTime == 0 {
		m.globalStats.MinTime = act.Duration
	}
	// 重新计算平均耗时
	if m.globalStats.TotalTasks > 0 {
		m.globalStats.AvgTime = m.globalStats.TotalTime / time.Duration(m.globalStats.TotalTasks)
	}
}

// GetWorkerStats 获取指定 worker 的统计（返回副本）
func (m *Monitor[T]) GetWorkerStats(workerID int) (WorkerStat, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := m.workerStats[workerID]
	if !ok {
		return WorkerStat{}, false
	}
	return *ws, true
}

// GetAllWorkerStats 获取所有 worker 统计
func (m *Monitor[T]) GetAllWorkerStats() map[int]WorkerStat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[int]WorkerStat, len(m.workerStats))
	for id, ws := range m.workerStats {
		result[id] = *ws
	}
	return result
}

// GetGlobalStats 获取全局统计（返回副本）
func (m *Monitor[T]) GetGlobalStats() GlobalStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.globalStats
}

// Stop 停止监控器（关闭内部的 goroutine）
func (m *Monitor[T]) Stop() {
	close(m.stopCh)
}
