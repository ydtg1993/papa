package engine

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// logSubmitError 框架自动记录提交类错误，避免业务漏记导致错误丢失；返回原 error 供调用方继续处理。
func (e *Engine) logSubmitError(task *Task, err error) error {
	e.loggerSet.Engine.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	return err
}

// validateTask 提交前的最小校验（stage/url/site 必填、阶段已登记）。
//
// 抽出来给 SubmitTask 与 submitRecoveredTask 共用：两条路对入参的要求必须一致 ——
// 恢复/轮询队列从库里重建的 Task 也可能因为历史脏行缺少字段，不能因为省一次查库就少校验一道。
func (e *Engine) validateTask(task *Task) error {
	if task.Stage == "" || task.URL == "" || task.Site == "" {
		return fmt.Errorf("task stage or url or site is empty: %+v", task)
	}
	if info := e.stages[stageKeyOf(task)]; info == nil {
		return fmt.Errorf("invalid stage: %s（站点 %q 下没登记这个阶段 —— 阶段名只在本站内唯一，先确认 task.Site 是声明它的那个站，再看该站的阶段清单，见 docs/FETCHER_WRITING_GUIDE.md 7.1）", task.Stage, task.Site)
	}
	return nil
}

// SubmitTask 任务提交
func (e *Engine) submitTask(task *Task) error {
	if err := e.validateTask(task); err != nil {
		return e.logSubmitError(task, err)
	}

	key := task.Unique()
	var record models.CrawlerTask

	// 两阶段去重：先查内存缓存，miss 再查 DB（唯一索引 idx_site_stage_url_hash 兜底）
	if e.dedupCache.Get(key) {
		if !task.Repeatable && task.IdempotencyKey == "" {
			e.requestPendingRecovery(task)
			return nil // existing durable task
		}
		// 已入库的轮询任务：查找记录防止重复录入
		if err := e.findTaskRecord(task, &record); err != nil {
			return e.logSubmitError(task, fmt.Errorf("load repeat task record: %w", err))
		}
		task.ID = int(record.ID)
		task.Repeat = record.Repeat
		return e.submitIfActive(task, record)
	}

	err := e.findTaskRecord(task, &record)
	if err == nil {
		// DB 命中：复用已有记录（重新加入内存缓存）
		e.dedupCache.Add(key)
		wasNew := task.ID == 0
		task.ID = int(record.ID)
		task.Repeat = record.Repeat
		// 仅「新任务」去重跳过；已带 ID 的重提交（如 RecoverJob）需继续入队
		if !task.Repeatable && wasNew {
			e.requestPendingRecovery(task)
			return nil
		}
		return e.submitIfActive(task, record)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return e.logSubmitError(task, fmt.Errorf("query task dedup: %w", err))
	}

	// 全新任务：提交到 pool 前先插入数据库
	if ierr := task.Insert(e.db); ierr != nil {
		e.requestPendingRecovery(task)
		// 并发下撞唯一索引：另一 goroutine 已入库并将入队，此处回填 ID 后跳过，避免重复入队
		if rerr := e.findTaskRecordFallback(task, &record); rerr != nil {
			return e.logSubmitError(task, fmt.Errorf("insert crawler task to db failed: %w", ierr))
		}
		e.dedupCache.Add(key)
		task.ID = int(record.ID)
		task.Repeat = record.Repeat
		e.requestPendingRecovery(task)
		return nil
	}
	record = task.toModel()
	record.ID = uint(task.ID)
	task.Repeat = record.Repeat
	e.dedupCache.Add(key)
	return e.submitIfActive(task, record)
}

// findTaskRecord 按去重键查找已存在的任务记录；未找到返回 gorm.ErrRecordNotFound。
//
// 两条分支都**按唯一索引查**（查得到的前提是"查的键与索引的键一致"，否则插冲突后回查会空手而归、
// 把并发冲突报成插入失败）：
//   - 自定义幂等键 → `idempotency_hash`（唯一索引 idx_idempotency_hash；键原文 varchar(1000) 建不了索引）
//   - 没有自定义键 → `url_hash + stage + site`（唯一索引 idx_site_stage_url_hash）
//
// 只投影身份、状态与投递所需字段，避免读取累积错误等无关内容。
func (e *Engine) findTaskRecord(task *Task, record *models.CrawlerTask) error {
	if err := e.dedupScope(task).Select("id", "site", "stage", "status", "repeat", "meta", "not_before").First(record).Error; err != nil {
		return err
	}
	return e.validatePersistedTask(task, record)
}

// dedupScope 给出"查已存在行"的条件（抽出来是为了能用 DryRun 把 SQL 形状钉住 —— 假库不解析 WHERE）。
func (e *Engine) dedupScope(task *Task) *gorm.DB {
	q := e.db.Model(&models.CrawlerTask{})
	if h := models.IdempotencyKeyHash(task.IdempotencyKey); h != "" {
		return q.Where("idempotency_hash = ?", h)
	}
	return q.Where("url_hash = ? AND stage = ? AND site = ?", models.UrlHash(task.URL), task.Stage, task.Site)
}

// findTaskRecordFallback 插入撞了唯一索引之后**无条件**按两条唯一索引回查。
//
// 为什么不能复用 findTaskRecord：它只走"这次提交用的那条键"。冲突完全可能来自**另一条**索引 ——
// 最典型的是幂等键文本变过（老行还存着旧文本，新代码算出的哈希查不到它），而 (url_hash, stage, site)
// 那条索引照样拦下这次插入。这时若只按哈希回查就会空手而归，把"复用已有行"误报成插入失败。
// 所以先按哈希（若有自定义键），再按 (url_hash, stage, site)。
func (e *Engine) findTaskRecordFallback(task *Task, record *models.CrawlerTask) error {
	if h := models.IdempotencyKeyHash(task.IdempotencyKey); h != "" {
		if err := e.db.Model(&models.CrawlerTask{}).Select("id", "site", "stage").Where("idempotency_hash = ?", h).First(record).Error; err == nil {
			return e.validatePersistedTask(task, record)
		}
	}
	err := e.db.Model(&models.CrawlerTask{}).Select("id", "site", "stage").
		Where("url_hash = ? AND stage = ? AND site = ?", models.UrlHash(task.URL), task.Stage, task.Site).
		First(record).Error
	if err != nil {
		return err
	}
	return e.validatePersistedTask(task, record)
}

// submitIfActive 若任务尚未到终态（success/failed）则延迟投递或入队，否则静默跳过。
//
// 元数据持久化失败返回错误；已接受的任务仍由持久化恢复接管。
func (e *Engine) submitIfActive(task *Task, record models.CrawlerTask) error {
	if record.Status != models.TaskStatusPending {
		return nil
	}
	if record.NotBefore != nil {
		task.NotBefore = *record.NotBefore
		task.Delay = 0
	}
	stored := e.metaFromJSON(record.Meta)
	merged := make(map[string]string, len(stored)+len(task.Meta))
	for k, v := range stored {
		merged[k] = v
	}
	for k, v := range task.Meta {
		merged[k] = v
	}
	if len(task.Meta) > 0 && !maps.Equal(stored, merged) {
		if err := e.db.Model(&models.CrawlerTask{}).Where("id = ? AND status = ?", record.ID, models.TaskStatusPending).Update("meta", metaToJSON(merged)).Error; err != nil {
			e.requestPendingRecovery(task)
			return err
		}
	}
	task.Meta = merged
	if at := task.deliverAt(); !at.IsZero() && at.After(time.Now()) {
		e.enqueueDelayed(at, task, record)
		return nil
	}
	e.submitToPool(task, record)
	return nil
}

// submitToPool only transfers an already durable pending task. Admission itself
// never refreshes updated_at or increments a polling generation.
func (e *Engine) submitToPool(task *Task, record models.CrawlerTask) {
	if record.Status != models.TaskStatusPending {
		return
	}
	e.afterTaskPending(task)
}
func (e *Engine) submitPendingTask(task *Task) { e.afterTaskPending(task) }
func (e *Engine) afterTaskPending(task *Task) {
	ctx := task.submitContext
	if ctx == nil {
		ctx = e.lifecycleContext()
	}
	if err := e.deliverPending(ctx, task); err != nil {
		e.requestPendingRecovery(task)
	}
}
func (e *Engine) submitRecoveredTask(task *Task) error {
	if err := e.validateTask(task); err != nil {
		return err
	}
	e.dedupCache.Add(task.Unique())
	if at := task.deliverAt(); !at.IsZero() && at.After(time.Now()) {
		e.enqueueDelayed(at, task, models.CrawlerTask{ID: uint(task.ID)})
		return nil
	}
	e.submitPendingTask(task)
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
		"status":        models.TaskStatusProcessing,
		"urgent":        false,
		"retry_blocked": gorm.Expr("COALESCE(retry_blocked, FALSE)"),
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// submitToStage is used by durable administrative and governance transitions.
func (e *Engine) submitToStage(task *Task) error {
	if e.stages[stageKeyOf(task)] == nil {
		return fmt.Errorf("%w: %s", ErrStageNotRegistered, task.Stage)
	}
	ctx := task.submitContext
	if ctx == nil {
		ctx = e.lifecycleContext()
	}
	err := e.deliverPending(ctx, task)
	if err != nil {
		e.requestPendingRecovery(task)
	}
	// Durable pending remains accepted even when cancellation ends admission.
	return nil
}

// SubmitTasks 批量提交任务：先按 Unique 做批内去重，再批量入库、逐个入队。
// 同键保留首次出现的任务参数，重复项回填相同 ID 和 Repeat，不重复投递。
// 逐条插入失败时继续处理其它行，成功项照常投递，最后汇总返回失败信息。
func (e *Engine) submitTasks(tasks []*Task) error {
	if len(tasks) == 0 {
		return nil
	}
	for _, t := range tasks {
		if t.Stage == "" || t.URL == "" || t.Site == "" {
			return e.logSubmitError(t, fmt.Errorf("task stage or url or site is empty: %+v", t))
		}
		if info := e.stages[stageKeyOf(t)]; info == nil {
			return e.logSubmitError(t, fmt.Errorf("invalid stage: %s（站点 %q 下没登记这个阶段 —— 阶段名只在本站内唯一，先确认 task.Site 是声明它的那个站，再看该站的阶段清单，见 docs/FETCHER_WRITING_GUIDE.md 7.1）", t.Stage, t.Site))
		}
	}

	// 两阶段去重与 repeatable 处理，收集需入库/入队的任务
	var toInsert []*Task
	var toProcess []*Task
	batch := make(map[string]*Task, len(tasks))
	defer func() {
		// 同批重复项没有独立插入，但调用方仍可读取它对应的任务身份。
		for _, t := range tasks {
			if first := batch[t.Unique()]; first != nil && first != t {
				t.ID, t.Repeat = first.ID, first.Repeat
			}
		}
	}()
	for _, t := range tasks {
		key := t.Unique()
		if first, exists := batch[key]; exists {
			if first.Site != t.Site || first.Stage != t.Stage {
				return fmt.Errorf("%w: batch idempotency key spans multiple sites or stages", ErrTaskFlow)
			}
			continue
		}
		// 不写进全局缓存：只有后续落库成功，任务才真正归我们负责。
		// 批内先排除重复，避免本批插入的首条被后续重复项标成外部冲突。
		batch[key] = t
		if e.dedupCache.Get(key) {
			if !t.Repeatable && t.ID == 0 && t.IdempotencyKey == "" {
				e.requestPendingRecovery(t)
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
				wasNew := t.ID == 0
				t.ID = int(record.ID)
				if !t.Repeatable && wasNew {
					e.requestPendingRecovery(t)
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
	var conflicted, failed map[string]struct{}
	var submitErrors []error
	if len(toInsert) > 0 {
		var err error
		conflicted, failed, err = e.insertTasks(toInsert)
		if err != nil {
			submitErrors = append(submitErrors, fmt.Errorf("insert crawler tasks: %w", err))
		}
	}

	// 成功项逐个入队（含延迟投递），失败项不缓存、不投递，允许调用方以后重试。
	for _, t := range toProcess {
		if _, hit := failed[t.Unique()]; hit {
			continue
		}
		// 并发撞车的那几条：库里那行已经被另一路入库并入队了，别再入一次（与 SubmitTask 同一处理）
		if _, hit := conflicted[t.Unique()]; hit {
			e.requestPendingRecovery(t)
			e.dedupCache.Add(t.Unique())
			continue
		}
		var record models.CrawlerTask
		if err := e.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).First(&record).Error; err != nil {
			submitErrors = append(submitErrors, fmt.Errorf("reload task %d before enqueue: %w", t.ID, err))
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				// 已经落库的行不能因读库瞬态错误失去投递责任；通知持久化恢复后再投。
				if at := t.deliverAt(); !at.IsZero() && at.After(time.Now()) {
					// 尚未到期的任务仍进延迟堆，恢复时也不能提前执行。
					e.enqueueDelayed(at, t, models.CrawlerTask{ID: uint(t.ID)})
				} else {
					e.requestPendingRecovery(t)
				}
			}
			continue
		}
		if err := e.validatePersistedTask(t, &record); err != nil {
			submitErrors = append(submitErrors, err)
			continue
		}
		e.dedupCache.Add(t.Unique())
		if record.Status == models.TaskStatusSuccess || record.Status == models.TaskStatusFailed {
			continue
		}
		if err := e.submitIfActive(t, record); err != nil {
			submitErrors = append(submitErrors, err)
		}
	}
	err := errors.Join(submitErrors...)
	if err != nil {
		e.loggerSet.Engine.Errorf("submit tasks failed: %v", err)
	}
	return err
}

// insertTasks 批量入库并回填 ID，分别返回外部冲突、实际失败的去重键及汇总错误。
//
// 快路径就是一条多行 INSERT —— SubmitTasks 本来就是冲着减少 DB 往返来的。
//
// 但**一行冲突不该让整批（可能上万条）一条都进不去**，而且还直接返回错误、连队列都没进，
// 业务看到的是一整批任务凭空消失。所以批量失败就退到逐条插。
//
// 这里不去分辨"批量失败是不是因为重复键"：逐条那条路自己会分辨 —— 插不进去就按唯一索引
// `idx_site_stage_url_hash` 回查一下，查得到说明确实是并发撞车（别的 goroutine / 进程在这两步之间
// 把同样的 site|stage|url 写进去了），回填它的 ID、记进冲突集合；查不到则记录插入错误，
// 继续处理后面的行，最终汇总失败信息，成功行仍由调用方负责投递。
// 这么写也就不依赖 gorm 的 TranslateError（默认没开）去识别 MySQL 的 1062。
func (e *Engine) insertTasks(tasks []*Task) (map[string]struct{}, map[string]struct{}, error) {
	records := make([]models.CrawlerTask, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, t.toModel())
	}

	if err := e.db.Create(&records).Error; err == nil {
		for i, t := range tasks {
			t.ID = int(records[i].ID)
		}
		return nil, nil, nil
	}

	for _, t := range tasks {
		e.requestPendingRecovery(t)
	}
	e.loggerSet.Engine.Warnf("batch insert failed, falling back to per-row insert (%d tasks)", len(tasks))
	conflicted := make(map[string]struct{})
	failed := make(map[string]struct{})
	var insertErrors []error
	for _, t := range tasks {
		rec := t.toModel()
		ierr := e.db.Create(&rec).Error
		if ierr == nil {
			t.ID = int(rec.ID)
			continue
		}
		// 按两条唯一索引回查（哈希优先，其次 (url_hash, stage)）—— 冲突落在哪条都接得住，
		// 而只查一条时"键文本变过"那种情况会把它报成插入失败。
		var existed models.CrawlerTask
		if serr := e.findTaskRecordFallback(t, &existed); serr != nil {
			failed[t.Unique()] = struct{}{}
			insertErrors = append(insertErrors, fmt.Errorf("insert task %q: %w", t.URL, ierr))
			continue
		}
		t.ID = int(existed.ID)
		conflicted[t.Unique()] = struct{}{}
	}
	return conflicted, failed, errors.Join(insertErrors...)
}
