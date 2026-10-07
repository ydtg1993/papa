package engine

import (
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// logSubmitError 框架自动记录提交类错误，避免业务漏记导致错误丢失；返回原 error 供调用方继续处理。
func (e *Engine) logSubmitError(task *Task, err error) error {
	e.loggerSet.Engine.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	return err
}

// SubmitTask 任务提交
func (e *Engine) SubmitTask(task *Task) error {
	if task.Stage == "" || task.URL == "" {
		return e.logSubmitError(task, fmt.Errorf("task stage or url is empty: %+v", task))
	}
	if _, ok := e.cfg.Crawler.Stages[task.Stage]; !ok {
		return e.logSubmitError(task, fmt.Errorf("invalid stage: %s", task.Stage))
	}

	key := task.Unique()
	var record models.CrawlerTask

	// 两阶段去重：先查内存缓存，miss 再查 DB（唯一索引 idx_stage_url 兜底）
	if e.dedupCache.Get(key) {
		if !task.Repeatable {
			return nil // 去重命中：视为成功，无需重复处理
		}
		// 已入库的轮询任务：查找记录防止重复录入
		if err := e.findTaskRecord(task, &record); err != nil {
			return e.logSubmitError(task, fmt.Errorf("load repeat task record: %w", err))
		}
		task.ID = int(record.ID)
		return e.submitIfActive(task, record)
	}

	err := e.findTaskRecord(task, &record)
	if err == nil {
		// DB 命中：复用已有记录（重新加入内存缓存）
		e.dedupCache.Add(key)
		wasNew := task.ID == 0
		task.ID = int(record.ID)
		// 仅「新任务」去重跳过；已带 ID 的重提交（如 RecoverJob）需继续入队
		if !task.Repeatable && wasNew {
			return nil
		}
		return e.submitIfActive(task, record)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return e.logSubmitError(task, fmt.Errorf("query task dedup: %w", err))
	}

	// 全新任务：提交到 pool 前先插入数据库
	if ierr := task.Insert(e.db); ierr != nil {
		// 并发下撞唯一索引：另一 goroutine 已入库并将入队，此处回填 ID 后跳过，避免重复入队
		if rerr := e.findTaskRecord(task, &record); rerr != nil {
			return e.logSubmitError(task, fmt.Errorf("insert crawler task to db failed: %w", ierr))
		}
		e.dedupCache.Add(key)
		task.ID = int(record.ID)
		return nil
	}
	e.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record)
	e.dedupCache.Add(key)
	return e.submitIfActive(task, record)
}

// findTaskRecord 按去重键查找已存在的任务记录；未找到返回 gorm.ErrRecordNotFound。
// 幂等键优先，否则回退 stage+url（与 DB 唯一索引 idx_stage_url 一致）。
func (e *Engine) findTaskRecord(task *Task, record *models.CrawlerTask) error {
	q := e.db.Model(&models.CrawlerTask{})
	if task.IdempotencyKey != "" {
		q = q.Where("idempotency_key = ?", task.IdempotencyKey)
	} else {
		q = q.Where("url = ? AND stage = ?", task.URL, task.Stage)
	}
	return q.First(record).Error
}

// submitIfActive 若任务尚未到终态（success/failed）则延迟投递或入队，否则静默跳过。
func (e *Engine) submitIfActive(task *Task, record models.CrawlerTask) error {
	if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
		return nil
	}
	// 延迟投递：到点才入队，避免 worker 空等浪费并发位
	if at := task.deliverAt(); !at.IsZero() && at.After(time.Now()) {
		e.enqueueDelayed(at, task, record)
		return nil
	}
	if err := e.submitToPool(task, record); err != nil {
		return e.logSubmitError(task, err)
	}
	return nil
}

// submitToPool 将任务提交到对应阶段工作池，并更新数据库状态。
func (e *Engine) submitToPool(task *Task, record models.CrawlerTask) error {
	info := e.stages[task.Stage]
	if info == nil {
		return fmt.Errorf("invalid stage ,task: %+v", task)
	}
	// 先落库再入队：「已入队 ⇒ 行里是 pending」必须是不变量。
	// worker 取到任务时会以 status=待处理 为条件认领（claimTask），
	// 如果这里先入队后落库，中间那个窗口里取到任务的 worker（repeatable 重跑时
	// 行里还是"成功"）会被误判成"已被运营改动"而跳过执行。
	if task.Repeatable && task.ID != 0 {
		record.Repeat += 1
	}
	record.Status = models.TaskStatusPending
	e.db.Save(record)

	if err := e.submitTo(info, task); err != nil {
		if errors.Is(err, workerpool.ErrQueueFull) {
			// 队列达 75% 高水位：任务保持 pending（已入库），加入溢出列表由 drain 稍后回灌
			e.spilledCount.Add(1)
			e.spillTask(task)
			return nil
		}
		// 提交失败，回滚内存去重表和数据库状态
		e.dedupCache.Delete(task.Unique())
		record.Error += err.Error() + "\n"
		record.Status = models.TaskStatusFailed
		e.db.Save(&record)
		return err
	}
	return nil
}

// claimTask 把任务从「待处理」认领为「处理中」，成功才允许执行。
// 条件更新而非先读再写：运营在它被 worker 取走之前标了失败（或删了行）时，
// 这里拿不到行，任务就不再执行 —— 这正是「标失败」对排队中任务的拦截力。
// 顺带把 urgent 归零：加急是「排队位置」的概念，跑过一次就完成使命，
// 不清的话失败重投会让加急任务越积越多、快车道被老任务长期占住。
// 返回 (是否认领成功, 错误)：DB 抖动不当成"认领失败"，由调用方记日志后继续执行，
// 免得一次抖动让任务永远没人跑。
func (e *Engine) claimTask(task *Task) (bool, error) {
	if task.ID == 0 {
		return true, nil // 未落库的任务（理论上不该出现）：没有行可认领，直接执行
	}
	res := claimScope(e.db, uint(task.ID)).Updates(map[string]any{
		"status": models.TaskStatusProcessing,
		"urgent": false,
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// submitTo 按任务的加急标记选队列：加急走快车道，其余走常规队列。
// 池子本身不认识优先级（Tasker 接口只有 Unique），路由决定留在这里。
func (e *Engine) submitTo(info *stageInfo, task *Task) error {
	if task.Urgent {
		return info.workerPool.SubmitUrgent(task)
	}
	return info.workerPool.Submit(task)
}

// SubmitTasks 批量提交任务：一次性批量入库（减少 DB 往返），再逐个入队。
func (e *Engine) SubmitTasks(tasks []*Task) error {
	if len(tasks) == 0 {
		return nil
	}
	for _, t := range tasks {
		if t.Stage == "" || t.URL == "" {
			return e.logSubmitError(t, fmt.Errorf("task stage or url is empty: %+v", t))
		}
		if _, ok := e.cfg.Crawler.Stages[t.Stage]; !ok {
			return e.logSubmitError(t, fmt.Errorf("invalid stage: %s", t.Stage))
		}
	}

	// 两阶段去重与 repeatable 处理，收集需入库/入队的任务
	var toInsert []*Task
	var toProcess []*Task
	for _, t := range tasks {
		key := t.Unique()
		if e.dedupCache.Get(key) {
			if !t.Repeatable {
				continue // 去重命中：跳过
			}
			var record models.CrawlerTask
			if err := e.findTaskRecord(t, &record); err != nil {
				return e.logSubmitError(t, fmt.Errorf("load repeat task record: %w", err))
			}
			t.ID = int(record.ID)
		} else {
			var record models.CrawlerTask
			err := e.findTaskRecord(t, &record)
			switch {
			case err == nil:
				// DB 命中：复用已有记录
				e.dedupCache.Add(key)
				t.ID = int(record.ID)
				if !t.Repeatable {
					continue
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				if t.ID == 0 {
					toInsert = append(toInsert, t)
				}
			default:
				return e.logSubmitError(t, fmt.Errorf("query task dedup: %w", err))
			}
		}
		toProcess = append(toProcess, t)
	}

	// 批量入库（单条多行 INSERT），失败退到逐条
	var conflicted map[string]struct{}
	if len(toInsert) > 0 {
		var err error
		conflicted, err = e.insertTasks(toInsert)
		if err != nil {
			err = fmt.Errorf("insert crawler tasks: %w", err)
			e.loggerSet.Engine.Errorf("submit tasks failed: %v", err)
			return err
		}
	}

	// 逐个入队（含延迟投递）
	var errs []error
	for _, t := range toProcess {
		// 并发撞车的那几条：库里那行已经被另一路入库并入队了，别再入一次（与 SubmitTask 同一处理）
		if _, hit := conflicted[t.Unique()]; hit {
			e.dedupCache.Add(t.Unique())
			continue
		}
		e.dedupCache.Add(t.Unique())
		var record models.CrawlerTask
		e.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).First(&record)
		if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
			continue
		}
		if at := t.deliverAt(); !at.IsZero() && at.After(time.Now()) {
			e.enqueueDelayed(at, t, record)
			continue
		}
		if err := e.submitToPool(t, record); err != nil {
			e.logSubmitError(t, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// insertTasks 批量入库并回填 ID，返回其中「撞了唯一索引、复用了库里已有行」的那些任务的去重键。
//
// 快路径就是一条多行 INSERT —— SubmitTasks 本来就是冲着减少 DB 往返来的。
//
// 但**一行冲突不该让整批（可能上万条）一条都进不去**，而且还直接返回错误、连队列都没进，
// 业务看到的是一整批任务凭空消失。所以批量失败就退到逐条插。
//
// 这里不去分辨"批量失败是不是因为重复键"：逐条那条路自己会分辨 —— 插不进去就按唯一索引
// `idx_stage_url` 回查一下，查得到说明确实是并发撞车（别的 goroutine / 进程在这两步之间
// 把同样的 stage|url 写进去了），回填它的 ID、记进返回的集合；查不到说明插入是真的失败了，
// 原样把插入的那个错报出去（它比"没查到"更有信息量）。
// 这么写也就不依赖 gorm 的 TranslateError（默认没开）去识别 MySQL 的 1062。
func (e *Engine) insertTasks(tasks []*Task) (map[string]struct{}, error) {
	records := make([]models.CrawlerTask, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, t.toModel())
	}

	if err := e.db.Create(&records).Error; err == nil {
		for i, t := range tasks {
			t.ID = int(records[i].ID)
		}
		return nil, nil
	}

	e.loggerSet.Engine.Warnf("batch insert failed, falling back to per-row insert (%d tasks)", len(tasks))
	conflicted := make(map[string]struct{})
	for _, t := range tasks {
		rec := t.toModel()
		ierr := e.db.Create(&rec).Error
		if ierr == nil {
			t.ID = int(rec.ID)
			continue
		}
		// 按 (stage, url) 回查 —— 唯一索引就是这个，所以冲突只可能落在它上面。
		// 不复用 findTaskRecord：它优先按 IdempotencyKey 查，而那不是唯一索引，可能捞回另一行。
		var existed models.CrawlerTask
		if serr := e.db.Select("id").Where("url = ? AND stage = ?", t.URL, t.Stage).
			First(&existed).Error; serr != nil {
			return nil, fmt.Errorf("insert task %q: %w", t.URL, ierr)
		}
		t.ID = int(existed.ID)
		conflicted[t.Unique()] = struct{}{}
	}
	return conflicted, nil
}

// ReSubmitTask 已入库的非轮询任务进行重提交任务
func (e *Engine) ReSubmitTask(task *Task) error {
	if task.Stage == "" || task.URL == "" {
		return e.logSubmitError(task, fmt.Errorf("task stage or url is empty: %+v", task))
	}
	if _, ok := e.cfg.Crawler.Stages[task.Stage]; !ok {
		return e.logSubmitError(task, fmt.Errorf("invalid stage: %s", task.Stage))
	}
	var record models.CrawlerTask
	e.db.Model(&models.CrawlerTask{}).
		Where("url = ?", task.URL).
		Where("stage = ?", task.Stage).
		First(&record)
	if record.ID == 0 {
		return e.logSubmitError(task, fmt.Errorf("record not exists: %s", task.URL))
	}
	task.ID = int(record.ID)
	info := e.stages[task.Stage]
	if err := e.submitTo(info, task); err != nil {
		// 提交失败，回滚内存去重表和数据库状态
		e.dedupCache.Delete(task.Unique())
		record.Error += err.Error() + "\n"
		record.Status = models.TaskStatusFailed
		e.db.Save(&record)
		return e.logSubmitError(task, err)
	}
	return nil
}
