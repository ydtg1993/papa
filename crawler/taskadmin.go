package crawler

import (
	"errors"
	"fmt"

	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// 后台手动操作任务的错误。crawler 不依赖 UI 库，所以只给可判别的哨兵错误，
// 由上层（internal/tasksource）映射成 HTTP 状态码。
var (
	ErrTaskNotFound       = errors.New("任务不存在")
	ErrTaskTerminal       = errors.New("该任务已结束（成功或失败），无需再标记失败")
	ErrTaskProcessing     = errors.New("该任务正在处理中，请先标记失败或等它结束")
	ErrTaskChanged        = errors.New("该行已被他人修改，请刷新后重试")
	ErrStageNotRegistered = errors.New("该任务的阶段未注册")
	ErrTaskUrgent         = errors.New("该任务已经加急过了，或已被取走，刷新后再看")
	ErrTaskFinished       = errors.New("该任务已结束，加急没有意义（要重跑请用「重投」）")
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

// urgentScope 只对「还没加急过」的行生效，防手抖双击与两人同点（与 retryScope 用
// reprocess 当版本号同一思路：纯整数、加急必 +1，不用 updated_at 那种带格式/时区坑的值）。
func urgentScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND urgent = ?", id, false)
}

func markFailedScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status IN ?", id,
			[]models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing})
}

func deleteScope(db *gorm.DB, id uint) *gorm.DB {
	return db.Where("id = ? AND status <> ?", id, models.TaskStatusProcessing)
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

	task := &Task{
		ID:             int(t.ID),
		PID:            int(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Repeatable:     t.Repeatable == models.RepeatableYes,
		Urgent:         t.Urgent,
		IdempotencyKey: t.IdempotencyKey,
	}
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

	task := &Task{
		ID:             int(t.ID),
		PID:            int(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Repeatable:     t.Repeatable == models.RepeatableYes,
		Urgent:         true,
		IdempotencyKey: t.IdempotencyKey,
	}
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
	e.DelActiveTask(&Task{URL: t.URL, Stage: t.Stage, IdempotencyKey: t.IdempotencyKey})
	return nil
}
