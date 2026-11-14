package engine

import (
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"gorm.io/gorm"
)

// repeatMinTick 自动扫描的最短间隔：算出来的节拍会被钳到不小于它。
// 再密没有意义（每轮扫描本身要查两次库），而且会让 ticker 空转。写入侧（SetTaskRepeatInterval）
// 也按它校验，所以不会出现"设了 3 秒却按 10 秒跑"的静默取整。
const repeatMinTick = 10 * time.Second

// repeatQueueState 轮询队列（全局一条）的运行态：自己的锁（自动与手动不会互相插队）、
// 唤醒通道、累计计数。
type repeatQueueState struct {
	mu       sync.Mutex    // 串行化队列执行
	wake     chan struct{} // "数据变了，重算节拍"（容量 1，非阻塞发）
	repolled atomic.Int64  // 累计重投数（同时就是监控里那一行的"处理量"）
}

// repeatBaseQuery 可轮询查询的底：可轮询、已完成。
func (e *Engine) repeatBaseQuery() *gorm.DB { return e.withRepeatLimits(repeatBaseQueryOn(e.db)) }

// repeatBaseQueryOn 同上，但接显式的 *gorm.DB：让"不连库渲染 SQL"的测试能拼出同一条语句。
func repeatBaseQueryOn(db *gorm.DB) *gorm.DB {
	return compensationScope(db.Model(&models.CrawlerTask{}).
		Where("repeatable = ? AND status IN ?", models.RepeatableYes, repeatDueStatuses))
}

// withRepeatLimits 只用于轮询队列，不改变 repeatable，也不约束其它重投入口。
func (e *Engine) withRepeatLimits(q *gorm.DB) *gorm.DB {
	keys := make([]string, 0, len(e.stages))
	for key, info := range e.stages {
		if info.config.RepeatLimit > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	// Fold only identical stage/limit predicates. NOT ((site=a OR site=b)
	// AND stage=s AND repeat>=n) preserves the original SQL three-valued
	// logic, including NULL site/repeat, without repeating stage/limit tests.
	type limitKey struct {
		stage string
		limit int
	}
	type limitGroup struct {
		key   limitKey
		sites []string
	}
	groups := make([]limitGroup, 0, len(keys))
	indices := make(map[limitKey]int, len(keys))
	for _, key := range keys {
		info := e.stages[key]
		groupKey := limitKey{info.stage, info.config.RepeatLimit}
		index, exists := indices[groupKey]
		if !exists {
			index = len(groups)
			indices[groupKey] = index
			groups = append(groups, limitGroup{key: groupKey})
		}
		groups[index].sites = append(groups[index].sites, info.config.Site)
	}
	for _, group := range groups {
		if len(group.sites) == 1 {
			q = q.Where("NOT (site = ? AND stage = ? AND `repeat` >= ?)", group.sites[0], group.key.stage, group.key.limit)
		} else {
			q = q.Where("NOT (site IN ? AND stage = ? AND `repeat` >= ?)", group.sites, group.key.stage, group.key.limit)
		}
	}
	return q
}

// repeatDueStatuses 可轮询的两个终态。
var repeatDueStatuses = []models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}

// repeatQueueQuery 「到点」的可轮询任务。
//
// 到点 = 未排期（NextRepeatAt 为 NULL：从未被排期）或下次到点时间已过。
// 这是一条纯比较谓词，走 idx_repeat_due（repeatable, status, next_repeat_at）；周期本身在
// **写路径**换算成 NextRepeatAt（见 requeueRepeatTask 那条 UPDATE），读路径不做任何算术。
func (e *Engine) repeatQueueQuery() func() *gorm.DB {
	return func() *gorm.DB {
		return e.repeatBaseQuery().
			Where("next_repeat_at IS NULL OR next_repeat_at <= NOW()")
	}
}

// repeatForceQueueQuery 可轮询任务的**全量**查询（后台「轮询任务」总按钮用）：
// 与上面只差"不看周期"这一条。
func (e *Engine) repeatForceQueueQuery() func() *gorm.DB {
	return func() *gorm.DB { return e.repeatBaseQuery() }
}

// soonestRepeatIn 返回"离最早一条到点还差多久"，用来让 ticker 跟上各自的周期。
// 负数 = 已经有到点的了（立刻扫）。
//
// 只看 NextRepeatAt（判据列）：
//   - 终态的行：过去时间保留负等待，未排期使用 NOW；最终节拍钳到最短刻度。
//   - 跑着 / 排队中的行：只在**未来**才算数（它到点时多半已经跑完，扫描正好接上）；
//     已经过期的（任务跑得比自己的周期还久）直接排除 —— 它跑完之前投不出去，
//     让这种行参与只会把扫描钉在最短刻度上空转。
//
// 没有可轮询的行、或查库出错 → false，调用方退回配置节拍。
func (e *Engine) soonestRepeatIn() (time.Duration, bool) {
	ctx := e.queueContext()
	if ctx.Err() != nil {
		return 0, false
	}
	var row struct{ Epoch *int64 }
	if err := e.soonestRepeatQueryOn(e.db.WithContext(ctx)).Scan(&row).Error; err != nil {
		if ctx.Err() != nil {
			return 0, false
		}
		e.loggerSet.Engine.Warnf("soonest repeat query 查询失败，退回配置节拍：%s", err.Error())
		return 0, false
	}
	if ctx.Err() != nil || row.Epoch == nil {
		return 0, false
	}
	return time.Until(time.Unix(*row.Epoch, 0)), true
}

// RepollRepeatableTasks 重新投递「到点」的可轮询任务（success/failed），返回实际投递数量。
// 手动触发（后台「立即执行」）走的就是它，所以手动同样只扫到点的。
func (e *Engine) RepollRepeatableTasks() (int, error) {
	return e.repollRepeatQueue(false)
}

// ForceRepollRepeatableTasks 把**所有可轮询的已完成任务**都投一遍 —— **忽略周期**。
// 后台「轮询任务」总按钮走它：适合"刚改完参数，想立刻整体重跑一轮"。
//
// 它与到点那条走同一套重置语句，所以每条的"下次到点"会按各自周期往后推 —— 手动插一轮不会
// 把自动节奏打乱（下一跳该什么时候还是什么时候）。
func (e *Engine) ForceRepollRepeatableTasks() (int, error) {
	return e.repollRepeatQueue(true)
}

// repollRepeatQueue 跑一轮轮询队列（force = 忽略到点条件）。
func (e *Engine) repollRepeatQueue(force bool) (int, error) {
	st := e.repeatQueue
	st.mu.Lock()
	defer st.mu.Unlock()

	cfg := e.cfg.RepeatQueue
	query := e.repeatQueueQuery()
	if force {
		query = e.repeatForceQueueQuery()
	}
	e.beginQueueRun(QueueRepeat)
	n, err := e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, 0, e.requeueRepeatTask())
	e.endQueueRun(QueueRepeat, n, err)
	if e.queueContext().Err() == nil {
		e.sampleQueueBacklogOne(QueueRepeat, query)
	}
	return n, err
}

// repeatResetScope 重投前把这一行重置为 pending 的条件：**此刻它还得可轮询、且已完成**。
//
// 批次 SELECT 与这次重置之间有个窗口，运营可能刚好点了后台的「停轮询」（或「重投」/「删除」
// 已经接手了这一行）。这几列才是"它该不该被重投"的判据，所以这里再确认一次 —— 停就是停，
// 这一轮不再投它。
func repeatResetScope(db *gorm.DB, id uint) *gorm.DB {
	return taskstore.RepeatScope(db, id)
}

// requeueRepeatTask 将单条 repeatable 任务重置为 pending 并重新投递。
func (e *Engine) requeueRepeatTask() func(*models.CrawlerTask) bool {
	return e.requeueRepeatTaskIn
}

// requeueRepeatTaskIn 单条重投的实体。
func (e *Engine) requeueRepeatTaskIn(t *models.CrawlerTask) bool {
	if e.queueContext().Err() != nil {
		return false
	}
	if e.breakers.breakerSettlementBlocked(t.Site) {
		return false
	}
	if e.compensationBlockedReason(t, time.Now()) != "" {
		return false
	}
	// 站点也要看：阶段名只在本站内唯一，光按名字查会把另一站的同名阶段当成本站那个。
	if e.stages[stageKeyOfRecord(t)] == nil {
		e.loggerSet.Engine.Warnf("repeat queue: stage %s not registered, skip task %d", stageKeyOfRecord(t), t.ID)
		return false
	}
	// 通过带轮次条件的任务存储操作重置。
	// 下一轮计数与状态、排期同一条语句落库；重复投递/重启恢复不再加次数。
	// 0 行 = 这一行已经不该被重投了：跳过，不计数、也不标 failed —— 它不是"重投失败"。
	info := e.stages[stageKeyOfRecord(t)]
	limit := info.config.RepeatLimit
	if limit > 0 && t.Repeat >= limit {
		return false
	}
	res := taskstore.AdvanceRepeat(e.db.WithContext(e.queueContext()), t.ID, t.CompensationVersion, limit, int64(e.cfg.RepeatQueue.Interval/time.Second))
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("repeat queue: reset task %d: %s", t.ID, res.Error.Error())
		return false
	}
	if res.RowsAffected == 0 {
		return false
	}
	// Repeatable 显式置真：本队列捞出来的行本来就该是可轮询的，写死比照抄行上的值更少一层怀疑。
	task := e.taskFromRecord(t)
	task.Repeatable = true
	task.Repeat = t.Repeat + 1
	task.deliveryVersion.Repeat = task.Repeat
	task.repeatCounted = true
	// 刚做完条件重置，行已经是 pending：用恢复队列同一条路径投递，省掉 SubmitTask 的去重查库。
	if err := e.submitRecoveredTask(task); err != nil {
		e.loggerSet.Engine.Errorf("repeat queue: submit task %d: %s", t.ID, err.Error())
		e.markRequeueFailed(QueueRepeat, t.ID, err)
		return false
	}
	e.repeatQueue.repolled.Add(1)
	e.emit(events.Event{
		Type: events.TypeTaskRequeued, Queue: QueueRepeat, Source: "auto",
		Site: t.Site, Stage: t.Stage, TaskID: t.ID,
	})
	return true
}

// wakeRepeatQueue 让轮询队列立刻重算扫描节拍：新提交了带周期的任务、后台改了某条的周期、
// 或把某条「开轮询」之后调它。节拍是 pull 出来的（只在 tick 到点或收到信号时重算），
// 不叫这一声就要等当前那次 sleep 到期 —— 可能是一整个 interval（比如 2 小时）。
//
// 非阻塞：它只是个"提醒"，丢了也无所谓（下一个 tick 照样会重算）。
func (e *Engine) wakeRepeatQueue() {
	st := e.repeatQueue
	if st == nil {
		return
	}
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// repeatTickInterval 下一次扫描的节拍：0 = 不自动轮询（总开关关着，仅手动触发）。
//
// 节拍 = min(全局 interval, 离最早一条到点还差多久)，并钳进 [repeatMinTick, 全局 interval]。
// 查不到数据（没有可轮询的行 / 库抖动）→ 退回全局节拍。
func (e *Engine) repeatTickInterval() time.Duration {
	cfg := e.cfg.RepeatQueue
	if !cfg.Enabled || cfg.Interval <= 0 {
		return 0
	}
	d, ok := e.soonestRepeatIn()
	if !ok {
		return cfg.Interval
	}
	if d < repeatMinTick {
		d = repeatMinTick
	}
	if d > cfg.Interval {
		return cfg.Interval
	}
	return d
}

// repeatRepolledTotal 累计重投数之和（监控埋点 `repeat_repoll_total`）。
func (e *Engine) repeatRepolledTotal() int64 {
	if e.repeatQueue == nil {
		return 0
	}
	return e.repeatQueue.repolled.Load()
}

// startRepeatQueue 启动轮询队列 ticker（一条全局）。
func (e *Engine) startRepeatQueue() {
	st := e.repeatQueue
	if st == nil {
		return
	}
	e.runDynamicTicker(func() time.Duration { return e.repeatTickInterval() }, st.wake, func() {
		if n, err := e.RepollRepeatableTasks(); err != nil {
			e.loggerSet.Engine.Errorf("repeat queue: auto repoll: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Debugf("repeat queue: auto repolled %d tasks", n)
		}
	})
}
