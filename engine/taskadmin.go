package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// 后台手动操作任务的错误。crawler 不依赖 UI 库，所以只给可判别的哨兵错误，
// 由上层（admin/tasksource）映射成 HTTP 状态码。
var (
	ErrTaskNotFound       = errors.New("任务不存在")
	ErrTaskTerminal       = errors.New("该任务已结束（成功或失败），无需再标记失败")
	ErrTaskProcessing     = errors.New("该任务正在处理中，请先标记失败或等它结束")
	ErrTaskChanged        = errors.New("该行已被他人修改，请刷新后重试")
	ErrStageNotRegistered = errors.New("该任务的阶段未注册")
	ErrTaskUrgent         = errors.New("该任务已经加急过了，或已被取走，刷新后再看")
	ErrTaskFinished       = errors.New("该任务已结束，加急没有意义（要重跑请用「重投」）")
	ErrTaskRepeatOn       = errors.New("该任务的周期轮询已经开着")
	ErrTaskRepeatOff      = errors.New("该任务的周期轮询本来就没开")
	ErrRepeatIntervalBad  = errors.New("轮询周期要么是 0（跟全局），要么不小于 10 秒")
)

// 三个动作的 WHERE 条件抽成共用函数：production 和测试吃同一份，
// 测试用 GORM 的 DryRun/ToSQL 断言条件确实写进了语句，而不是"先查再写"。
func claimScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ?", id, models.TaskStatusPending)
}

func retryScope(db *gorm.DB, id uint, wasReprocess int) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status <> ? AND reprocess = ?", id, models.TaskStatusProcessing, wasReprocess)
}

// urgentScope 只对「还没加急过、且还没被取走」的行生效，防手抖双击与两人同点
// （与 retryScope 用 reprocess 当版本号同一思路：纯整数、加急必 +1，
// 不用 updated_at 那种带格式/时区坑的值）。
//
// **status 守卫不能少**：UrgentTask 开头那次 loadTask 只是快照，这一行可能在
// SELECT 与 UPDATE 之间被 worker 认领 —— 而 claimTask 认领时恰好把 urgent 清成 0，
// 只写 `urgent = false` 的话这里照样命中，于是把 urgent=1 留在一个已经在跑（甚至已结束）
// 的行上，且再没有任何路径会清掉它；之后 error_queue / 后台「重投」还会读这个陈旧标记，
// 让一次无关的重投莫名其妙插队。带上 status 后这种竞态落回 RowsAffected=0 → ErrTaskUrgent。
func urgentScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ? AND urgent = ?", id, models.TaskStatusPending, false)
}

func markFailedScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status IN ?", id,
			[]models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing})
}

func deleteScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Where("id = ? AND status <> ?", id, models.TaskStatusProcessing)
}

// setRepeatableScope 开/停周期轮询的条件：这一列当前得是**要被换掉**的那个值
// （要开就必须现在关着，要停就必须现在开着）—— 这一列本身就是版本守卫，
// 手抖双击或两人同点时只有第一次能匹配上，与 retryScope 用 reprocess 当版本号同一思路。
//
// **WHERE 里刻意不带 status**（与 urgentScope 相反）：停轮询最常见的用法恰恰是
// "这条正在跑，跑完这次别再轮询了"，带 status 守卫会把最该支持的那种情况挡掉。
// 轮询开关与任务处在哪个状态无关 —— 它只决定"完成后要不要再被捞起来"。
func setRepeatableScope(db *gorm.DB, id uint, was models.RepeatableStatus) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("id = ? AND repeatable = ?", id, was)
}

// setRepeatIntervalScope 改轮询周期的条件：这一列当前得是**行快照里那个旧值**
// （同 retryScope 用 reprocess 当版本号同一思路：重复点击只有第一次能匹配上）。
func setRepeatIntervalScope(db *gorm.DB, id uint, wasSeconds int) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).Where("id = ? AND repeat_interval = ?", id, wasSeconds)
}

// loadTask 按 ID 取一行任务；不存在时返回 ErrTaskNotFound。
func (e *Engine) loadTask(id uint) (*models.CrawlerTask, error) {
	var t models.CrawlerTask
	err := e.db.First(&t, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load task %d: %w", id, err)
	}
	return &t, nil
}

// RetryTask 把一行任务重置为「待处理」并重新投递到它所属阶段的池。
// 语义与 error_queue 的单行重投一致：重试次数归零、代次 +1。
//
// wasReprocess 是调用方从行快照里带回来的 reprocess 旧值，作为版本条件：
// 手抖双击或两个人同时点时，只有第一次能匹配上，后一次拿到 ErrTaskChanged。
// 这里不用 updated_at 当版本号 —— 它从客户端回来是 RFC3339 字符串，
// 直接和 MySQL 的 datetime 比会有格式/时区坑；reprocess 是纯整数且重投必 +1。
func (e *Engine) RetryTask(id uint, wasReprocess int) error {
	t, err := e.loadTask(id)
	if err != nil {
		return err
	}
	info := e.stages[t.Stage]
	if info == nil || info.workerPool == nil {
		return fmt.Errorf("%w: %s", ErrStageNotRegistered, t.Stage)
	}

	res := retryScope(e.db, id, wasReprocess).
		Updates(map[string]any{
			"status":    models.TaskStatusPending,
			"retry":     0,
			"reprocess": gorm.Expr("reprocess + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("reset task %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return e.whyRetryRejected(id)
	}

	task := e.taskFromRecord(t)
	task.Retry = 0 // 与 error_queue 的单行重投同语义：行里 retry 刚归零，内存副本跟上
	e.dedupCache.Add(task.Unique())
	if err := e.submitTo(info, task); err != nil {
		// 队列达高水位：与正常投递一致，溢出到 DB 由 drain 回灌，不算失败
		if errors.Is(err, workerpool.ErrQueueFull) {
			e.spilledCount.Add(1)
			e.spillTask(task)
			return nil
		}
		e.dedupCache.Delete(task.Unique())
		return fmt.Errorf("submit task %d: %w", id, err)
	}
	return nil
}

// whyRetryRejected 条件更新影响 0 行时，再查一次把原因说清楚。
func (e *Engine) whyRetryRejected(id uint) error {
	t, err := e.loadTask(id)
	if err != nil {
		return err // 行不存在
	}
	if t.Status == models.TaskStatusProcessing {
		return ErrTaskProcessing
	}
	return ErrTaskChanged
}

// UrgentTask 给一行「还没被取走」的任务加急：另投一份到所属阶段的快车道，插到常规队列前面。
//
// 条件更新 `WHERE id = ? AND urgent = 0` 当版本守卫（与 RetryTask 用 reprocess 同一思路）：
// 手抖双击或两个人同时点时只有第一次能匹配上。
//
// 已知竞态（可接受）：这条任务本来就排在常规队列里时，池里会有同一行的两份副本，
// 靠 claimTask 的条件更新仲裁 —— 谁先认领谁执行，另一份被跳过。窗口很小、后果自愈。
func (e *Engine) UrgentTask(id uint) error {
	t, err := e.loadTask(id)
	if err != nil {
		return err
	}
	if t.Status == models.TaskStatusProcessing {
		return ErrTaskUrgent // 已经被 worker 取走了，快车道帮不上忙
	}
	if t.Status == models.TaskStatusSuccess || t.Status == models.TaskStatusFailed {
		return ErrTaskFinished
	}
	info := e.stages[t.Stage]
	if info == nil || info.workerPool == nil {
		return fmt.Errorf("%w: %s", ErrStageNotRegistered, t.Stage)
	}

	// 先落库再加急：这样后台「加急」列看得见，任务溢出回灌或进程重启后也还认得出它加急过。
	res := urgentScope(e.db, id).Update("urgent", true)
	if res.Error != nil {
		return fmt.Errorf("mark task %d urgent: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrTaskUrgent
	}

	// Urgent 必须显式置真：上面的条件更新改的是库里的行，这里读到的 t 是更新**之前**的快照。
	task := e.taskFromRecord(t)
	task.Urgent = true
	e.dedupCache.Add(task.Unique())
	if err := e.submitTo(info, task); err != nil {
		// 与正常投递一致：队列满就溢出到 DB 由 drain 回灌，不算失败
		if errors.Is(err, workerpool.ErrQueueFull) {
			e.spilledCount.Add(1)
			e.spillTask(task)
			return nil
		}
		e.dedupCache.Delete(task.Unique())
		return fmt.Errorf("submit urgent task %d: %w", id, err)
	}
	return nil
}

// SetTaskRepeatable 运行期开/停一条任务的周期轮询：只改这一列，**不**顺手重投一次 ——
// 开与停都在下一轮 repeat_queue 扫描时生效（想立刻跑一次用「重投」）。
// 这一列原先只有首次插入时写（`Task.toModel`），所以"提交后再想改"没有任何路径。
//
// 刻意不先 loadTask：这里不需要快照里的任何字段（不像 UrgentTask 要拿 Stage/Status 做前置判断），
// 影响 0 行时交给 whyRepeatRejected 冷路径再查一次，把原因说清楚。
func (e *Engine) SetTaskRepeatable(id uint, on bool) error {
	want, was := models.RepeatableYes, models.RepeatableNo
	if !on {
		want, was = models.RepeatableNo, models.RepeatableYes
	}
	updates := map[string]any{"repeatable": want}
	if on {
		// 「开轮询」= 下一轮扫描就投它（REPEAT_QUEUE.md 第 3 节那句）：把排期直接置为"现在"。
		// 不置的话，刚被停过又开的任务要白等一个周期；而从未排期过的行本来也是"NULL = 到点"。
		updates["next_repeat_at"] = gorm.Expr("NOW()")
	}
	res := setRepeatableScope(e.db, id, was).Updates(updates)
	if res.Error != nil {
		return fmt.Errorf("set task %d repeatable=%v: %w", id, on, res.Error)
	}
	if res.RowsAffected == 0 {
		return e.whyRepeatRejected(id, on)
	}
	if on {
		e.wakeRepeatQueue() // 别让它等到当前那次 sleep 到期才被看见
	}
	return nil
}

// SetTaskRepeatInterval 改一条任务的轮询周期（秒；0 = 跟全局）。只改周期相关的列，不顺手重投
// （想立刻跑一次用「重投」）。合法值：0，或 ≥ repeatMinTick（写入侧就挡住"设了 3 秒却按 10 秒跑"）。
//
// wasSeconds 是行快照里的旧值，当版本条件（同 RetryTask 用 reprocess）：手抖双击或两人同点时
// 只有第一次能匹配上。改周期**连带重算 NextRepeatAt** —— 否则它还按旧周期排着（1h 改成 10m，
// 却还要等 55 分钟）；基准取"上次轮询时刻"（不是 NOW()），所以改短之后可能立刻就该跑；
// 从未轮询过（LastRepeatAt 为空）就从这一刻起算。
func (e *Engine) SetTaskRepeatInterval(id uint, wasSeconds, seconds int) error {
	if seconds < 0 || (seconds > 0 && seconds < int(repeatMinTick/time.Second)) {
		return ErrRepeatIntervalBad
	}
	eff := int64(seconds)
	if eff == 0 {
		eff = int64(e.repeatQueueConfig().Interval / time.Second) // 0 = 跟全局
	}
	res := setRepeatIntervalScope(e.db, id, wasSeconds).Updates(map[string]any{
		"repeat_interval": seconds,
		// 时间换算留在库里：写入端与判据端（next_repeat_at <= NOW()）用同一个时钟
		"next_repeat_at": gorm.Expr(
			"FROM_UNIXTIME(UNIX_TIMESTAMP(COALESCE(last_repeat_at, NOW())) + ?)", eff),
	})
	if res.Error != nil {
		return fmt.Errorf("set task %d repeat_interval=%d: %w", id, seconds, res.Error)
	}
	if res.RowsAffected == 0 {
		if _, err := e.loadTask(id); err != nil {
			return err // 行不存在
		}
		return ErrTaskChanged // 周期刚被改过（快照过期），刷新后再看
	}
	e.wakeRepeatQueue()
	return nil
}

// whyRepeatRejected 条件更新影响 0 行时，再查一次把原因说清楚。
func (e *Engine) whyRepeatRejected(id uint, on bool) error {
	t, err := e.loadTask(id)
	if err != nil {
		return err // 行不存在
	}
	if now := t.Repeatable == models.RepeatableYes; now == on {
		// 这一列已经是目标值：重复点击落在这儿
		if on {
			return ErrTaskRepeatOn
		}
		return ErrTaskRepeatOff
	}
	// 有人在中间改过又改回来 —— 这一方输了，与 whyRetryRejected 的兜底同一语义
	return ErrTaskChanged
}

// MarkTaskFailed 把「还没结束」的任务标记为失败（待处理/处理中都算）。
// 允许标记待处理的行，是为了让运营能在 worker 取走之前拦下一条排队任务：
// worker 执行前会用 claimTask 以 status=待处理 为条件认领，拿不到行就不执行。
func (e *Engine) MarkTaskFailed(id uint, reason string) error {
	msg := "后台手动标记失败"
	if reason != "" {
		msg += "：" + reason
	}
	res := markFailedScope(e.db, id).
		Updates(map[string]any{
			"status": models.TaskStatusFailed,
			"error":  gorm.Expr("CONCAT(COALESCE(error, ''), ?)", msg+"\n"),
		})
	if res.Error != nil {
		return fmt.Errorf("mark task %d failed: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		if _, err := e.loadTask(id); err != nil {
			return err // 行不存在
		}
		return ErrTaskTerminal
	}
	return nil
}

// DeleteTask 删除一行任务，并清掉它在内存去重表里的残留。
// 处理中的行拒绝删除：worker 稍后会对已删行 UpdateStatus，且运营侧看到"行消失又冒出新行"很困惑。
func (e *Engine) DeleteTask(id uint) error {
	t, err := e.loadTask(id) // 先取 URL/Stage，删除后就没法算去重键了
	if err != nil {
		return err
	}
	if t.Status == models.TaskStatusProcessing {
		return ErrTaskProcessing
	}
	res := deleteScope(e.db, id).Delete(&models.CrawlerTask{})
	if res.Error != nil {
		return fmt.Errorf("delete task %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		if _, err := e.loadTask(id); err != nil {
			return err // 行不存在
		}
		return ErrTaskProcessing
	}
	// 这里只需要算去重键（Unique 只认幂等键或 stage|url），不必把整行还原成 Task。
	e.DelActiveTask(&Task{URL: t.URL, Stage: t.Stage, IdempotencyKey: t.IdempotencyKey})
	return nil
}
