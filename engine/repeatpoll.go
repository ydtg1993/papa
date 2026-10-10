package engine

import (
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// repeatMinTick 自动扫描的最短间隔：算出来的节拍会被钳到不小于它。
// 再密没有意义（每轮扫描本身要查两次库），而且会让 ticker 空转。写入侧（SetTaskRepeatInterval
// 与后台「设轮询周期」）也按它校验，所以不会出现"设了 3 秒却按 10 秒跑"的静默取整。
const repeatMinTick = 10 * time.Second

// repeatQueueQuery 周期轮询的查询条件：**已到点**的可轮询任务（已完成）。
//
// 到点 = 未排期（NextRepeatAt 为 NULL：迁移前的老行、或从未被轮询过）或下次到点时间已过。
// 这是一条纯比较谓词，走 idx_repeat_due（repeatable, status, next_repeat_at）；周期本身在
// **写路径**换算成 NextRepeatAt（见 requeueRepeatTask 那条 UPDATE），读路径不做任何算术 ——
// 函数谓词（`TIMESTAMPDIFF(last_repeat_at, NOW()) >= 周期` 那类）会让索引失效，而且本仓没有
// 真库测试，SQL 里的算术验不了。
//
// Model 不能省 —— 理由同 errorQueueQuery：`sampleQueueBacklogOne` 的 `Count(&n)`
// 推不出表名，没有 Model 就会静默把积压数永远留成 0。
func (e *Engine) repeatQueueQuery() func() *gorm.DB {
	return func() *gorm.DB {
		return e.db.Model(&models.CrawlerTask{}).
			Where("repeatable = ? AND status IN ?",
				models.RepeatableYes,
				[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}).
			Where("next_repeat_at IS NULL OR next_repeat_at <= NOW()")
	}
}

// soonestRepeatIn 返回"离最早一条到点还差多久"，用来让 ticker 跟上各自的周期。
// 负数 = 已经有到点的了（立刻扫）。
//
// 只看 NextRepeatAt（判据列）：
//   - 终态的行（会被真扫描）：已到点 / 未排期 → 0；否则它自己的到点时间。
//   - 跑着 / 排队中的行：只在**未来**才算数（它到点时多半已经跑完，扫描正好接上）；
//     已经过期的（任务跑得比自己的周期还久）直接排除 —— 它跑完之前投不出去，
//     让这种行参与只会把扫描钉在最短刻度上空转。
//
// 没有可轮询的行、或查库出错 → false，调用方退回全局节拍（DB 抖动不该让 ticker 崩，也不该刷日志）。
func (e *Engine) soonestRepeatIn() (time.Duration, bool) {
	var row struct{ Epoch *int64 }
	err := e.db.Model(&models.CrawlerTask{}).
		Where("repeatable = ?", models.RepeatableYes).
		Select(`MIN(CASE
			WHEN status IN ? THEN UNIX_TIMESTAMP(COALESCE(next_repeat_at, NOW()))
			WHEN next_repeat_at > NOW() THEN UNIX_TIMESTAMP(next_repeat_at)
			ELSE NULL END) AS epoch`,
			[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed}).
		Scan(&row).Error
	if err != nil || row.Epoch == nil {
		return 0, false
	}
	return time.Until(time.Unix(*row.Epoch, 0)), true
}

// RepollRepeatableTasks 重新投递「到点」的可轮询任务（success/failed），返回实际投递数量。
// 只重投 success/failed，不碰还在 pending/processing 的——后者由 recover_queue 兜底，避免双入队。
// 手动触发（后台「立即执行」/ `POST /api/repeatqueue/process`）走的也是它，所以手动同样只扫到点的：
// 想强制某一条立刻重跑，用任务表的「重投」。
// 分页流式 + 并发处理（复用 processInBatches），并发数与批大小取 repeat_queue 配置。
func (e *Engine) RepollRepeatableTasks() (int, error) {
	e.repeatQueueMu.Lock()
	defer e.repeatQueueMu.Unlock()

	cfg := e.repeatQueueConfig()
	query := e.repeatQueueQuery()
	e.beginQueueRun(QueueRepeat)
	n, err := e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueRepeatTask)
	e.endQueueRun(QueueRepeat, n, err)
	e.sampleQueueBacklogOne(QueueRepeat, query)
	return n, err
}

// repeatResetScope 重投前把这一行重置为 pending 的条件：**此刻它还得是可轮询的、且已完成**。
//
// 批次 SELECT 与这次重置之间有个窗口，运营可能刚好点了后台的「停轮询」（或「重投」/「删除」
// 已经接手了这一行）。这一列才是"它该不该被重投"的唯一判据，所以这里再确认一次 —— 停就是停，
// 这一轮不再投它。status 一并带上：别人刚把它重置成 pending 时，这里也不该再插一脚。
func repeatResetScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND repeatable = ? AND status IN ?", id, models.RepeatableYes,
			[]models.TaskStatus{models.TaskStatusSuccess, models.TaskStatusFailed})
}

// requeueRepeatTask 将单条 repeatable 任务重置为 pending 并重新投递。
func (e *Engine) requeueRepeatTask(t *models.CrawlerTask) bool {
	if e.stages[t.Stage] == nil {
		e.loggerSet.Engine.Warnf("repeat queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	// 条件重置（不用 task.UpdateStatus：它无条件、且被 worker 等多处共用，不往它上面加条件）。
	// 一条语句把三个字段一起写：状态、记录（上次轮询时刻）、判据（下次到点）。
	// 0 行 = 这一行已经不该被重投了：跳过，不计数、也不标 failed —— 它不是"重投失败"。
	res := repeatResetScope(e.db, uint(t.ID)).Updates(map[string]any{
		"status":         models.TaskStatusPending,
		"last_repeat_at": gorm.Expr("NOW()"),
		// 下次到点 = 现在 + 自己的周期（没定就用全局）。换算只在这一条写路径上做一次，
		// 用 UNIX_TIMESTAMP 加法而不是 DATE_ADD(..., INTERVAL <表达式> SECOND)：后者把函数塞进
		// INTERVAL 的写法有语法/优化器上的余地，而本仓没有真库测试去验。时间一律取库里的 NOW()：
		// 写入端与判据端同一个时钟，不受应用与数据库时钟偏差影响。
		"next_repeat_at": gorm.Expr(
			"FROM_UNIXTIME(UNIX_TIMESTAMP(NOW()) + COALESCE(NULLIF(repeat_interval, 0), ?))",
			int64(e.repeatQueueConfig().Interval/time.Second)),
	})
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
	if err := e.SubmitTask(task); err != nil {
		e.loggerSet.Engine.Errorf("repeat queue: submit task %d: %s", t.ID, err.Error())
		// SubmitTask 内部（submitToPool）已经失败标过 failed 了，这一句管的是它提前返回的那些路
		e.markRequeueFailed(QueueRepeat, t.ID, err)
		return false
	}
	e.repeatRepolledCount.Add(1)
	return true
}

// wakeRepeatQueue 让轮询队列立刻重算扫描节拍：新提交了带周期的任务、后台改了某条的周期、
// 或把某条「开轮询」之后调它。节拍是 pull 出来的（只在 tick 到点或收到信号时重算），
// 不叫这一声就要等当前那次 sleep 到期 —— 可能是一整个全局 interval（比如 2 小时）。
//
// 非阻塞：它只是个"提醒"，丢了也无所谓（下一个 tick 照样会重算）。手搓的 Engine（测试里）
// 没建这个 channel，nil channel 在 select 里永远走 default，安全。
func (e *Engine) wakeRepeatQueue() {
	select {
	case e.repeatWake <- struct{}{}:
	default:
	}
}

// repeatTickInterval 下一次扫描的节拍：0 = 不自动轮询（总开关关着，仅手动触发）。
//
// 节拍 = min(全局 interval, 离最早一条到点还差多久)，并钳进 [repeatMinTick, 全局 interval]：
// 全局 interval 从"每条任务的周期"变成"最粗兜底" —— 任务自己定的 10 分钟就真是 10 分钟，
// 而新提交的行、被改过周期的行最多等这么久被发现（等不到时由 wakeRepeatQueue 叫醒）。
// 查不到数据（没有可轮询的行 / 库抖动）→ 退回全局节拍，DB 抖动不该让 ticker 崩。
func (e *Engine) repeatTickInterval() time.Duration {
	cfg := e.repeatQueueConfig()
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

// startRepeatQueue 启动后台定时重新投递到点的 repeatable 任务（enabled/interval 运行期可热更）。
func (e *Engine) startRepeatQueue() {
	e.runDynamicTicker(e.repeatTickInterval, e.repeatWake, func() {
		if n, err := e.RepollRepeatableTasks(); err != nil {
			e.loggerSet.Engine.Errorf("repeat queue: auto repoll: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("repeat queue: auto repolled %d tasks", n)
		}
	})
}
