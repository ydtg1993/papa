package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"gorm.io/gorm"
	"time"
)

type submissionLogger interface {
	Warnf(string, ...any)
	Errorf(string, ...any)
}
type taskReferenceGuard interface {
	ProtectReferences(context.Context, []account.PoolKey, func() error) error
}

// submissionStore owns identity lookup, reference-protected inserts and ownership
// adoption. Dedup is an injected shared acceleration cache, not durable authority.
// Each call returns committed delivery responsibilities, including read failures.
type submissionStore struct {
	db         *gorm.DB
	ctx        context.Context
	dedupCache *dedupCache
	references taskReferenceGuard
	logger     submissionLogger
}
type submissionDelivery struct {
	task   *Task
	record models.CrawlerTask
	unread bool
	batch  bool
}
type submissionPlan struct {
	// Report single-task admission errors only after delivery responsibility is retained.
	logFailure bool
	deliveries []submissionDelivery
	originals  []*Task
	first      map[string]*Task
}

func (p *submissionPlan) deliver(task *Task, record models.CrawlerTask, batch bool) {
	p.deliveries = append(p.deliveries, submissionDelivery{task: task, record: record, batch: batch})
}
func (p *submissionPlan) retain(task *Task, batch bool) {
	p.deliveries = append(p.deliveries, submissionDelivery{task: task, unread: true, batch: batch})
}

// Backfill after delivery so duplicate callers also observe repeat initialization.
func (p submissionPlan) backfill() {
	for _, t := range p.originals {
		if first := p.first[t.Unique()]; first != nil && first != t {
			t.ID, t.Repeat = first.ID, first.Repeat
			t.repeatCounted = first.repeatCounted
		}
	}
}
func (s submissionStore) logSubmitError(task *Task, err error) error {
	if task != nil {
		s.logger.Errorf("submit task failed: stage=%q url=%q err=%v", task.Stage, task.URL, err)
	}
	return err
}

func (s submissionStore) protectTaskPoolReferences(tasks []*Task, insert func() error) error {
	var keys []account.PoolKey
	for _, task := range tasks {
		if task.AccountPool != "" {
			keys = append(keys, account.PoolKey{Site: task.Site, Name: task.AccountPool})
		}
	}
	if len(keys) == 0 {
		return insert()
	}
	if s.references == nil {
		return account.ErrPoolUnknown
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return s.references.ProtectReferences(ctx, keys, insert)
}

func (s submissionStore) findTaskRecord(task *Task, record *models.CrawlerTask) error {
	return s.dedupScope(task).Select("id", "status", "repeat", "reprocess", "site", "stage", "account_pool", "url", "not_before", "delay_ms").First(record).Error
}

func (s submissionStore) dedupScope(task *Task) *gorm.DB {
	return s.db.Model(&models.CrawlerTask{}).Where("site = ? AND stage = ? AND identity_hash = ?", task.Site, task.Stage, task.Unique())
}

func (s submissionStore) findTaskRecordFallback(task *Task, record *models.CrawlerTask) error {
	return s.findTaskRecord(task, record)
}

func (s submissionStore) insertTasks(tasks []*Task) (map[string]struct{}, map[string]struct{}, error) {
	var conflicted, failed map[string]struct{}
	err := s.protectTaskPoolReferences(tasks, func() error { var err error; conflicted, failed, err = s.insertTasksWithReferences(tasks); return err })
	if err != nil && failed == nil {
		failed = map[string]struct{}{}
		for _, task := range tasks {
			failed[task.Unique()] = struct{}{}
		}
	}
	return conflicted, failed, err
}

func (s submissionStore) insertTasksWithReferences(tasks []*Task) (map[string]struct{}, map[string]struct{}, error) {
	records := make([]models.CrawlerTask, 0, len(tasks))
	for _, t := range tasks {
		records = append(records, t.toModel())
	}

	if err := s.db.Create(&records).Error; err == nil {
		for i, t := range tasks {
			t.ID = int(records[i].ID)
			t.NotBefore = taskTimeValue(records[i].NotBefore)
			t.deliveryDurable = true
		}
		return nil, nil, nil
	}

	s.logger.Warnf("batch insert failed, falling back to per-row insert (%d tasks)", len(tasks))
	conflicted := make(map[string]struct{})
	failed := make(map[string]struct{})
	var insertErrors []error
	for _, t := range tasks {
		rec := t.toModel()
		ierr := s.db.Create(&rec).Error
		if ierr == nil {
			t.ID = int(rec.ID)
			t.NotBefore = taskTimeValue(rec.NotBefore)
			t.deliveryDurable = true
			continue
		}
		// 只按标准身份回查；不同 ID 的相同 URL 不能相互吞掉。
		var existed models.CrawlerTask
		if serr := s.findTaskRecordFallback(t, &existed); serr != nil {
			failed[t.Unique()] = struct{}{}
			insertErrors = append(insertErrors, fmt.Errorf("insert task %q: %w", t.URL, ierr))
			continue
		}
		if err := adoptTaskOwnership(t, &existed); err != nil {
			failed[t.Unique()] = struct{}{}
			insertErrors = append(insertErrors, err)
			continue
		}
		t.ID = int(existed.ID)
		conflicted[t.Unique()] = struct{}{}
	}
	return conflicted, failed, errors.Join(insertErrors...)
}

func (s submissionStore) prepare(task *Task) (out submissionPlan, resultErr error) {
	key := task.Unique()
	var record models.CrawlerTask

	// 两阶段去重：先查内存缓存，miss 再查 DB（唯一索引 idx_task_identity 兜底）
	if s.dedupCache.Get(key) {
		if !task.Repeatable && task.AccountPool == "" {
			return out, nil // 去重命中：视为成功，无需重复处理
		}
		// 已入库的轮询任务：查找记录防止重复录入
		if err := s.findTaskRecord(task, &record); err != nil {
			out.logFailure = true
			return out, fmt.Errorf("load repeat task record: %w", err)
		}
		if err := adoptTaskOwnership(task, &record); err != nil {
			return out, err
		}
		wasNew := task.ID == 0
		task.NotBefore = taskTimeValue(record.NotBefore)
		task.Delay = time.Duration(record.DelayMS) * time.Millisecond
		task.ID = int(record.ID)
		task.Repeat = record.Repeat
		if !task.Repeatable && wasNew {
			return out, nil
		}
		out.deliver(task, record, false)
		return out, nil
	}

	err := s.findTaskRecord(task, &record)
	if err == nil {
		if err := adoptTaskOwnership(task, &record); err != nil {
			return out, err
		}
		// DB 命中：复用已有记录（重新加入内存缓存）
		s.dedupCache.Add(key)
		wasNew := task.ID == 0
		task.NotBefore = taskTimeValue(record.NotBefore)
		task.Delay = time.Duration(record.DelayMS) * time.Millisecond
		task.ID = int(record.ID)
		task.Repeat = record.Repeat
		// 仅「新任务」去重跳过；已带 ID 的重提交（如 RecoverJob）需继续入队
		if !task.Repeatable && wasNew {
			return out, nil
		}
		out.deliver(task, record, false)
		return out, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		out.logFailure = true
		return out, fmt.Errorf("query task dedup: %w", err)
	}

	// A new task must be durable before returning its delivery responsibility.
	if ierr := s.protectTaskPoolReferences([]*Task{task}, func() error { return task.Insert(s.db) }); ierr != nil {
		// 并发下撞唯一索引：另一 goroutine 已入库并将入队，此处回填 ID 后跳过，避免重复入队
		if rerr := s.findTaskRecordFallback(task, &record); rerr != nil {
			out.logFailure = true
			return out, fmt.Errorf("insert crawler task to db failed: %w", ierr)
		}
		if err := adoptTaskOwnership(task, &record); err != nil {
			return out, err
		}
		s.dedupCache.Add(key)
		task.ID = int(record.ID)
		task.Repeat = record.Repeat
		return out, nil
	}
	if err := s.db.Model(&models.CrawlerTask{}).Where("id = ?", task.ID).First(&record).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			out.retain(task, false)
		}
		out.logFailure = true
		return out, fmt.Errorf("reload inserted task %d: %w", task.ID, err)
	}
	s.dedupCache.Add(key)
	out.deliver(task, record, false)
	return out, nil
}

// prepareBatch transfers each durable row synchronously before reading the next.
// accept is a call-scoped responsibility handoff, never retained by the store.
func (s submissionStore) prepareBatch(tasks []*Task, accept func(submissionDelivery)) (out submissionPlan, resultErr error) {
	// Reject conflicting ownership within the batch before writing any rows.
	owners := map[string]*Task{}
	for _, t := range tasks {
		if prior := owners[t.Unique()]; prior != nil {
			if prior.Site != t.Site || prior.Stage != t.Stage || prior.AccountPool != t.AccountPool {
				return out, ErrTaskOwnershipConflict
			}
		} else {
			owners[t.Unique()] = t
		}
	}

	// 两阶段去重与 repeatable 处理，收集需入库/入队的任务
	var toInsert []*Task
	var toProcess []*Task
	batch := make(map[string]*Task, len(tasks))
	out.originals, out.first = tasks, batch
	defer out.backfill()
	for _, t := range tasks {
		key := t.Unique()
		if _, exists := batch[key]; exists {
			continue
		}
		// 不写进全局缓存：只有后续落库成功，任务才真正归我们负责。
		// 批内先排除重复，避免本批插入的首条被后续重复项标成外部冲突。
		batch[key] = t
		if s.dedupCache.Get(key) {
			if !t.Repeatable && t.AccountPool == "" {
				continue // 去重命中：跳过
			}
			var record models.CrawlerTask
			if err := s.findTaskRecord(t, &record); err != nil {
				return out, s.logSubmitError(t, fmt.Errorf("load repeat task record: %w", err))
			}
			if err := adoptTaskOwnership(t, &record); err != nil {
				return out, err
			}
			t.ID = int(record.ID)
			t.NotBefore = taskTimeValue(record.NotBefore)
			t.Delay = time.Duration(record.DelayMS) * time.Millisecond
			if !t.Repeatable {
				continue
			}
		} else {
			var record models.CrawlerTask
			err := s.findTaskRecord(t, &record)
			switch {
			case err == nil:
				t.NotBefore = taskTimeValue(record.NotBefore)
				t.Delay = time.Duration(record.DelayMS) * time.Millisecond
				if err := adoptTaskOwnership(t, &record); err != nil {
					return out, err
				}
				// DB 命中：复用已有记录
				s.dedupCache.Add(key)
				t.ID = int(record.ID)
				if !t.Repeatable {
					continue
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				if t.ID == 0 {
					toInsert = append(toInsert, t)
				}
			default:
				return out, s.logSubmitError(t, fmt.Errorf("query task dedup: %w", err))
			}
		}
		toProcess = append(toProcess, t)
	}

	// 批量入库（单条多行 INSERT），失败退到逐条
	var conflicted, failed map[string]struct{}
	var submitErrors []error
	if len(toInsert) > 0 {
		var err error
		conflicted, failed, err = s.insertTasks(toInsert)
		if err != nil {
			submitErrors = append(submitErrors, fmt.Errorf("insert crawler tasks: %w", err))
		}
	}

	// Successful rows become delivery responsibilities; failures remain retryable.
	for _, t := range toProcess {
		if _, hit := failed[t.Unique()]; hit {
			continue
		}
		// 并发撞车的那几条：库里那行已经被另一路入库并入队了，别再入一次（与 SubmitTask 同一处理）
		if _, hit := conflicted[t.Unique()]; hit {
			s.dedupCache.Add(t.Unique())
			continue
		}
		s.dedupCache.Add(t.Unique())
		var record models.CrawlerTask
		if err := s.db.Model(&models.CrawlerTask{}).Where("id = ?", t.ID).First(&record).Error; err != nil {
			submitErrors = append(submitErrors, fmt.Errorf("reload task %d before enqueue: %w", t.ID, err))
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				accept(submissionDelivery{task: t, unread: true, batch: true})
			}
			continue
		}
		accept(submissionDelivery{task: t, record: record, batch: true})
	}
	err := errors.Join(submitErrors...)
	if err != nil {
		s.logger.Errorf("submit tasks failed: %v", err)
	}
	return out, err
}

func adoptTaskOwnership(task *Task, record *models.CrawlerTask) error {
	if (record.Site != "" && record.Site != task.Site) || (record.Stage != "" && record.Stage != task.Stage) || (task.AccountPool != "" && task.AccountPool != record.AccountPool) {
		return ErrTaskOwnershipConflict
	}
	task.AccountPool = record.AccountPool
	if record.URL != "" && (task.ResourceID != "" || task.IdempotencyKey != "") {
		task.URL = record.URL
	}
	return nil
}

// preparePending only changes columns owned by delivery, guarded against claims.
func (s submissionStore) preparePending(task *Task, id uint) error {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	scope := s.db.WithContext(ctx).Model(&models.CrawlerTask{}).Where("id = ? AND status = ?", id, models.TaskStatusPending)
	if task.deliveryVersion != nil {
		scope = scope.Where("`repeat` = ? AND reprocess = ?", task.deliveryVersion.Repeat, task.deliveryVersion.Reprocess)
	}
	err := scope.Updates(submitUpdates(task)).Error
	if err == nil {
		task.deliveryDurable = true
	}
	return err
}

// claim returns a rejected row snapshot so the scheduler can respect not_before.
func (s submissionStore) claim(ctx context.Context, id uint, versions ...*taskRunVersion) (bool, *models.CrawlerTask, error) {
	scope := s.db.WithContext(ctx)
	if len(versions) > 0 && versions[0] != nil {
		scope = scope.Where("`repeat` = ? AND reprocess = ?", versions[0].Repeat, versions[0].Reprocess)
	}
	result := taskstore.Claim(scope, id)
	if result.Error != nil {
		return false, nil, result.Error
	}
	if result.RowsAffected != 0 {
		return result.RowsAffected == 1, nil, nil
	}
	var row models.CrawlerTask
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return false, &row, nil
}

// submitTo 按任务的加急标记选队列：加急走快车道，其余走常规队列。
// 池子本身不认识优先级（Tasker 接口只有 Unique），路由决定留在这里。
// submitUpdates 一次提交要写回行里的列 —— **只写这个函数负责的那几列**（见 submitToPool 的注释）。
//
// 抽出来是为了能在**不连库**的前提下把拼出来的 SQL 渲染出来验（engine/sqlshape_test.go）：
// `repeat` 是 MySQL 保留字，裸写进表达式会被当成 REPEAT() 函数调用、整条 UPDATE 报 1064 ——
// 这个坑真踩过（v3.1.0 起，只有真库跑得出来，假库测试看不见）。
func submitUpdates(task *Task) map[string]any {
	updates := map[string]any{"status": models.TaskStatusPending}
	if task.Repeatable && task.ID != 0 && !task.repeatCounted {
		// 这里只初始化首轮；并发/顺序重复提交同一 pending 行都保持原计数。
		// 真正的下一轮在 repeat queue 的终态→pending 条件更新中原子递增。
		updates["repeat"] = gorm.Expr("CASE WHEN `repeat` = 0 THEN 1 ELSE `repeat` END")
	}
	// 这次提交带了 Meta 就刷新这一列（带空 Meta 的提交不动它）。
	// 行是这条任务的记录，而重投路径全靠它还原身份 —— 不刷新的话库里会一直留着上一次的身份，
	// 下一次重投就把任务带回旧的业务行上（SubmitTask/SubmitTasks 的 Meta 只是内存里的，
	// 谁也看不出来它和库里的不一致）。
	if len(task.Meta) > 0 {
		updates["meta"] = metaToJSON(task.Meta)
	}
	// 站点同理：行是这条任务的记录，重投路径全靠它认出"这条属于哪个站"
	if task.Site != "" {
		updates["site"] = task.Site
	}
	return updates
}
