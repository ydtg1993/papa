package crawler

import (
	"sync"
	"time"

	"gorm.io/gorm"
)

// 治理队列名称，同时作为监控快照的 key。
const (
	QueueError   = "error_queue"
	QueueRecover = "recover_queue"
	QueueRepeat  = "repeat_queue"
)

// defaultQueueSampleInterval 积压采样的默认间隔。
const defaultQueueSampleInterval = time.Minute

// QueueStat 单个治理队列的运行快照（纯值类型，供监控页读取，不暴露内部实现）。
type QueueStat struct {
	Name           string        `json:"name"`
	Enabled        bool          `json:"enabled"`         // 是否启用自动轮询
	Running        bool          `json:"running"`         // 本轮是否正在执行
	Runs           int64         `json:"runs"`            // 累计执行次数
	StartedAt      time.Time     `json:"started_at"`      // 本轮开始时间（Running 时有意义）
	LastFinishAt   time.Time     `json:"last_finish_at"`  // 上次执行完成时间
	LastDuration   time.Duration `json:"last_duration"`   // 上次执行耗时
	LastProcessed  int           `json:"last_processed"`  // 上次实际重新投递的任务数
	RunProcessed   int64         `json:"run_processed"`   // 本轮已重新投递的任务数（Running 时递增）
	TotalProcessed int64         `json:"total_processed"` // 累计重新投递的任务数
	Backlog        int           `json:"backlog"`         // 待处理积压数（最近一次采样值）
	BacklogAt      time.Time     `json:"backlog_at"`      // 积压采样时间
	LastError      string        `json:"last_error"`      // 上次执行错误，空=正常
}

// queueRunState 队列运行期状态：快照 + 本轮起点计数。
type queueRunState struct {
	mu      sync.Mutex
	stat    QueueStat
	atStart int64 // 本轮开始时的累计处理数，用于推算本轮进度
}

// newQueueRuns 构造三个治理队列的运行状态表。
func newQueueRuns() map[string]*queueRunState {
	return map[string]*queueRunState{
		QueueError:   {},
		QueueRecover: {},
		QueueRepeat:  {},
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

// GetQueueStats 返回三个治理队列的运行快照（只读内存，不查库，供监控页高频拉取）。
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
func (e *Engine) queueEnabled(name string) bool {
	switch name {
	case QueueError:
		return e.errorQueueConfig().Enabled
	case QueueRecover:
		return e.recoverQueueConfig().Enabled
	case QueueRepeat:
		return e.repeatQueueConfig().Enabled
	}
	return false
}

// queueQueries 返回三个队列各自的「待处理」条件构造器。
func (e *Engine) queueQueries() map[string]func() *gorm.DB {
	return map[string]func() *gorm.DB{
		QueueError:   e.errorQueueQuery(),
		QueueRecover: e.recoverQueueQuery(),
		QueueRepeat:  e.repeatQueueQuery(),
	}
}

// sampleQueueBacklog 采样三个队列的待处理积压数（各一次 COUNT），写入快照。
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

// startQueueSampler 后台低频采样三个队列的积压数（COUNT 查询）。
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
