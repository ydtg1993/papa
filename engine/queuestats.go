package engine

import (
	"sync"
	"time"

	"gorm.io/gorm"
)

// 治理队列名称，同时作为监控快照的 key。
// 只有这两个是**运行期持续跑**的队列，所以只有它们进监控面板。
const (
	QueueError  = "error_queue"
	QueueRepeat = "repeat_queue"
)

// recoverQueueName 启动恢复在日志与错误信息里的标识。
// 它不再是监控面板上的「队列」—— 恢复只在启动那一刻发生一次，没有周期、没有积压可看 ——
// 但重新投递失败时仍要写清是哪条路写的（见 markRequeueFailed）。
const recoverQueueName = "recover_queue"

// defaultQueueSampleInterval 积压采样的默认间隔。
const defaultQueueSampleInterval = time.Minute

// queueRunState 队列运行期状态：快照 + 本轮起点计数。
type queueRunState struct {
	mu      sync.Mutex
	stat    QueueStat
	atStart int64 // 本轮开始时的累计处理数，用于推算本轮进度
}

// newQueueRuns 构造治理队列的运行状态表。
func newQueueRuns() map[string]*queueRunState {
	return map[string]*queueRunState{
		QueueError:  {},
		QueueRepeat: {},
	}
}

// beginQueueRun 标记队列本轮开始执行。
func (e *Engine) beginQueueRun(name string) {
	st := e.queueRuns[name]
	if st == nil {
		return
	}
	var total int64
	if c := e.queueCounters[name]; c != nil {
		total = c.Load()
	}
	st.mu.Lock()
	st.stat.Running = true
	st.stat.StartedAt = time.Now()
	st.atStart = total
	st.mu.Unlock()
}

// endQueueRun 记录队列本轮执行结果（processed 为实际重新投递成功的任务数）。
func (e *Engine) endQueueRun(name string, processed int, err error) {
	st := e.queueRuns[name]
	if st == nil {
		return
	}
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stat.Running = false
	st.stat.Runs++
	st.stat.LastProcessed = processed
	st.stat.LastFinishAt = now
	st.stat.LastDuration = now.Sub(st.stat.StartedAt)
	if err != nil {
		st.stat.LastError = err.Error()
	} else {
		st.stat.LastError = ""
	}
}

// setQueueBacklog 记录采样到的待处理积压数。
func (e *Engine) setQueueBacklog(name string, backlog int) {
	st := e.queueRuns[name]
	if st == nil {
		return
	}
	st.mu.Lock()
	st.stat.Backlog = backlog
	st.stat.BacklogAt = time.Now()
	st.mu.Unlock()
}

// GetQueueStats 返回各治理队列的运行快照（只读内存，不查库，供监控页高频拉取）。
func (e *Engine) GetQueueStats() map[string]QueueStat {
	out := make(map[string]QueueStat, len(e.queueRuns))
	for name, st := range e.queueRuns {
		var total int64
		if c := e.queueCounters[name]; c != nil {
			total = c.Load()
		}
		st.mu.Lock()
		s := st.stat
		atStart := st.atStart
		st.mu.Unlock()

		s.Name = name
		s.Enabled = e.queueEnabled(name)
		s.TotalProcessed = total
		if s.Running {
			s.RunProcessed = total - atStart
		} else {
			s.RunProcessed = 0
		}
		out[name] = s
	}
	return out
}

// queueEnabled 返回队列是否启用自动轮询（读生效配置，含运行期覆盖）。
//
// 周期轮询队列是**按站点**分的：那一行的开关 = 全局 `repeat_queue.enabled` **且** 本站
// `AutoRepeat`（站点级只关得掉自己那一份）。站点名必须是登记过的 —— 不认识的名字一律 false，
// 免得拼错的站名在后台多出一行"看起来开着"的假队列。
func (e *Engine) queueEnabled(name string) bool {
	switch name {
	case QueueError:
		return e.errorQueueConfig().Enabled
	}
	if site, ok := e.repeatQueueSite(name); ok {
		return e.repeatQueueConfig().Enabled && e.siteAutoRepeat(site)
	}
	return false
}

// queueQueries 返回各队列自己的「待处理」条件构造器（每条固定队列 + 每个站点一条轮询队列）。
func (e *Engine) queueQueries() map[string]func() *gorm.DB {
	out := map[string]func() *gorm.DB{QueueError: e.errorQueueQuery()}
	for _, site := range e.repeatQueueSites() {
		out[repeatQueueKey(site)] = e.repeatQueueQuery(site)
	}
	return out
}

// sampleQueueBacklog 采样各队列的待处理积压数（各一次 COUNT），写入快照。
func (e *Engine) sampleQueueBacklog() {
	for name, query := range e.queueQueries() {
		e.sampleQueueBacklogOne(name, query)
	}
}

// sampleQueueBacklogOne 采样单个队列的待处理积压数（一次 COUNT）。
// 查询失败只记日志：积压数属于观测数据，不影响队列本身。
func (e *Engine) sampleQueueBacklogOne(name string, query func() *gorm.DB) {
	var n int64
	if err := query().Count(&n).Error; err != nil {
		e.loggerSet.Engine.Warnf("queue backlog sample %s: %s", name, err.Error())
		return
	}
	e.setQueueBacklog(name, int(n))
}

// startQueueSampler 后台低频采样各队列的积压数（COUNT 查询）。
// 监控页刷新只读内存快照，采样间隔由 server.queue_sample_interval 控制（默认 1m）。
func (e *Engine) startQueueSampler() {
	interval := e.cfg.Server.QueueSampleInterval
	if interval <= 0 {
		interval = defaultQueueSampleInterval
	}
	go func() {
		e.sampleQueueBacklog()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.sampleQueueBacklog()
			}
		}
	}()
}
