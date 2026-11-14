package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/ydtg1993/papa/v4/core"

	"github.com/ydtg1993/papa/v4/models"
)

var ErrTaskOwnershipConflict = errors.New("existing task belongs to another site, stage or account pool")

// logSubmitError 框架自动记录提交类错误，避免业务漏记导致错误丢失；返回原 error 供调用方继续处理。
func (e *Engine) logSubmitError(task *Task, err error) error {
	if task == nil {
		return err
	}
	e.loggerSet.Engine.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	return err
}

// validateTask 提交前的最小校验（stage/url/site 必填、阶段已登记）。
//
// 抽出来给 SubmitTask 与 submitRecoveredTask 共用：两条路对入参的要求必须一致 ——
// 恢复/轮询队列从库里重建的 Task 也可能因为历史脏行缺少字段，不能因为省一次查库就少校验一道。
func (e *Engine) validateTask(task *Task) error {
	if task == nil || task.Stage == "" || task.URL == "" || task.Site == "" {
		return fmt.Errorf("task stage or url or site is empty: %+v", task)
	}
	if task.Delay < 0 {
		return fmt.Errorf("task delay must not be negative")
	}
	if info := e.stages[stageKeyOf(task)]; info == nil {
		return fmt.Errorf("invalid stage: %s（站点 %q 下没登记这个阶段 —— 阶段名只在本站内唯一，先确认 task.Site 是声明它的那个站，再看该站的阶段清单，见 docs/guides/FETCHER_WRITING_GUIDE.md 7.1）", task.Stage, task.Site)
	}
	resourceID := task.ResourceID

	if _, err := core.NormalizeTaskIdentity(task.Site, task.Stage, resourceID, task.URL); err != nil {
		return err
	}

	return nil
}

// submitIfActive 若任务尚未到终态（success/failed）则延迟投递或入队，否则静默跳过。
//
// 没有返回值：已受理任务由持久记录接管，延迟或暂不可投递时仍保留 pending，
// 随后由阶段投递器按容量继续交接。
func (e *Engine) submitIfActive(task *Task, record models.CrawlerTask) {
	if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
		return
	}
	// 延迟投递：到点才入队，避免 worker 空等浪费并发位
	if record.NotBefore != nil {
		task.NotBefore = *record.NotBefore
		task.Delay = time.Duration(record.DelayMS) * time.Millisecond
	}
	if at := task.NotBefore; !at.IsZero() && at.After(time.Now()) {
		e.enqueueDelayed(at, task, record)
		return
	}
	e.submitToPool(task, record)
}

// submitToPool publishes durable pending work without refreshing its state.
func (e *Engine) submitToPool(task *Task, record models.CrawlerTask) {
	if record.Status != models.TaskStatusPending {
		e.residents.release(task, "")
		return
	}
	e.afterTaskPending(task)
}
func (e *Engine) submitPendingTask(task *Task) { e.afterTaskPending(task) }
func (e *Engine) afterTaskPending(task *Task) {
	if task.Urgent {
		if pool := e.pools.Get(stageKeyOf(task)); pool != nil {
			pool.Promote(func(queued *Task) bool { return queued.ID == task.ID })
		}
	}
	e.residents.release(task, "")
	e.requestPendingRecovery(task)
	if task.RepeatInterval > 0 {
		e.wakeRepeatQueue()
	}
}
func (e *Engine) submitRecoveredTask(task *Task) error {
	if err := e.validateTask(task); err != nil {
		return err
	}
	e.afterTaskPending(task)
	return nil
}

// claimTask 把任务从「待处理」认领为「处理中」，成功才允许执行。
// 条件更新而非先读再写：运营在它被 worker 取走之前标了失败（或删了行）时，
// 这里拿不到行，任务就不再执行 —— 这正是「标失败」对排队中任务的拦截力。
// 顺带把 urgent 归零：加急是「排队位置」的概念，跑过一次就完成使命，
// 不清的话失败重投会让加急任务越积越多、快车道被老任务长期占住。
// 返回 (是否认领成功, 错误)：出错不能当作执行许可；worker 保留责任并重试认领。
func (e *Engine) claimTask(task *Task) (bool, error) {
	return e.claimTaskContext(context.Background(), task)
}

func (e *Engine) claimTaskContext(ctx context.Context, task *Task) (bool, error) {
	if task.FlowID != "" {
		return e.claimFlowTask(ctx, task)
	}
	if task.ID == 0 {
		return true, nil // 未落库的任务（理论上不该出现）：没有行可认领，直接执行
	}
	claimed, row, err := e.submissions().claim(ctx, uint(task.ID), task.deliveryVersion)
	if err != nil {
		return false, err
	}
	if row != nil && task.deliveryVersion != nil && (task.deliveryVersion.Repeat != row.Repeat || task.deliveryVersion.Reprocess != row.Reprocess) {
		return false, nil
	}
	if row != nil && row.Status == models.TaskStatusPending && row.NotBefore != nil {
		task.NotBefore = *row.NotBefore
		at := time.Now().Add(time.Second)
		if task.NotBefore.After(at) {
			at = task.NotBefore
		}
		e.enqueueDelayed(at, task, *row)
	}
	return claimed, nil
}

func (e *Engine) submissions() submissionStore {
	var references taskReferenceGuard
	if m := e.GetAccountManager(); m != nil && m.Pools() != nil {
		references = m.Pools()
	}
	var logger submissionLogger
	if e.loggerSet != nil {
		logger = e.loggerSet.Engine
	}
	return submissionStore{db: e.db, ctx: e.ctx, dedupCache: e.dedupCache, references: references, logger: logger}
}

// SubmitTask validates and persists one task before accepting delivery responsibility.
func (e *Engine) submitTask(task *Task) error {
	if err := e.beginTaskAdmission(); err != nil {
		return err
	}
	defer e.endTaskAdmission()

	if task != nil && task.FlowID != "" {
		return ErrFlowInvalid
	}
	if err := e.validateTask(task); err != nil {
		return e.logSubmitError(task, err)
	}
	if task.ID == 0 {
		task.AccountPool = e.resolveTaskPool(task)
	}
	if e.residents.state(task) != "" {
		return nil
	}
	reserved := e.residents.admit(task, "submission")

	plan, err := e.submissions().prepare(task)
	if reserved {
		e.residents.admit(task, "submission")
	}
	err = errors.Join(err, e.deliverSubmission(plan))
	e.residents.release(task, "submission")
	if plan.logFailure {
		return e.logSubmitError(task, err)
	}
	return err
}

// SubmitTasks retains successful rows even when other members of the batch fail.
// Duplicate inputs receive the first task's persisted identity and repeat state.
func (e *Engine) submitTasksContext(ctx context.Context, tasks []*Task) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(e.queueContext(), cancel)
	defer func() { stop(); cancel() }()
	if e.queueContext().Err() != nil {
		cancel()
	}
	if err := e.beginTaskAdmission(); err != nil {
		return err
	}
	defer e.endTaskAdmission()

	for _, task := range tasks {
		if task != nil && task.FlowID != "" {
			return ErrFlowInvalid
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	for _, task := range tasks {
		if err := e.validateTask(task); err != nil {
			return err
		}
		if task.ID == 0 {
			task.AccountPool = e.resolveTaskPool(task)
		}
	}
	var deliveryErrors []error
	store := e.submissions()
	store.ctx = ctx
	store.db = store.db.WithContext(ctx)
	_, err := store.prepareBatch(tasks, func(delivery submissionDelivery) {
		if err := e.deliverSubmission(submissionPlan{deliveries: []submissionDelivery{delivery}}); err != nil {
			deliveryErrors = append(deliveryErrors, err)
		}
	})
	return errors.Join(append(deliveryErrors, err)...)
}

// Delivery transfers accepted rows to the durable dispatcher. Metadata supplied
// by this call is persisted before acknowledging it, never retained in overflow.
func (e *Engine) deliverSubmission(plan submissionPlan) error {
	var errs []error
	for _, d := range plan.deliveries {
		task := d.task
		if !d.unread {
			if d.record.Status != models.TaskStatusPending {
				e.residents.release(task, "")
				continue
			}
			task.deliveryVersion = &taskRunVersion{Repeat: d.record.Repeat, Reprocess: d.record.Reprocess}
			stored := e.metaFromJSON(d.record.Meta)
			merged := make(map[string]string, len(stored)+len(task.Meta))
			for k, v := range stored {
				merged[k] = v
			}
			for k, v := range task.Meta {
				merged[k] = v
			}
			if len(task.Meta) > 0 && !maps.Equal(stored, merged) {
				task.Meta = merged
				if err := e.submissions().preparePending(task, uint(task.ID)); err != nil {
					errs = append(errs, err)
				}
			}
		}
		e.afterTaskPending(task)
	}
	return errors.Join(errs...)
}
