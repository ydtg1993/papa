package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// markRequeueFailed 把「重新投递失败」写回行上：标 failed、retry+1、追加原因。
//
// 三个治理队列（error / recover / repeat）共用它。之前 error_queue 和 repeat_queue 在这条路上
// 只 log 不管，行停在 pending —— 而它们的捞取条件是 status=failed（error_queue）与
// status IN (success, failed)（repeat_queue），于是这条任务再也不会被任何一个队列捞起来：
// 运营看着是"排队中"，实际早就没人会执行它了。
//
// 只改变仍 pending 的行：已失败不重复追加，已被另一个投递认领或已完成的行也不能覆盖。
func (e *Engine) markRequeueFailed(queue string, id uint, err error) {
	updates := taskStatusUpdates(models.TaskStatusFailed, fmt.Errorf("%s 重新投递失败：%w", queue, err))
	updates["retry"] = gorm.Expr("retry + 1")
	res := e.db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status = ?", id, models.TaskStatusPending).
		Updates(updates)
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("%s: mark task %d failed: %s", queue, id, res.Error.Error())
	}
}

// processInBatches 分页查询并并发处理任务，避免一次性全量加载到内存（海量失败/卡死任务时防内存尖峰）。
// query 返回带条件的查询（不含 Order/Limit）；requeue 处理单条并返回是否成功；
// pause 是**批间停顿**（0 = 不停）：只在"这一批满了、可能还有下一批"时生效，用来把海量
// 启动恢复摊到多个时间片，而不是在启动瞬间把各阶段队列灌爆。停机信号可立即打断停顿。
//
// 分页游标是**自己上一批的最大 id**（keyset），不是 OFFSET、也不能指望「处理过的行会离开结果集」。
// 后者是原来的写法，它一直没出事只是因为 recover_queue 那条 `updated_at < cutoff` 顺手把行
// 踢出了结果集 —— 而启动恢复把 processing 改成 pending 后，行**依然满足**「未到终态」，
// 再 `Limit` 查一遍就是原地打转、永远跑不完（而且它持着队列锁 + 手动触发的 HTTP 请求）。
// 改成 keyset 之后三个队列都是「每条最多看一次」：跑不动的那些（阶段没注册之类）跳过就跳过，不成环。
func (e *Engine) processInBatches(query func() *gorm.DB, batchSize, workers int, pause time.Duration, requeue func(*models.CrawlerTask) bool) (int, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if workers <= 0 {
		workers = 1
	}
	var (
		total  int
		lastID uint // keyset 游标：上一批的最大 id
	)
	ctx := e.queueContext()
	for {
		if ctx.Err() != nil {
			return total, nil
		}
		var tasks []models.CrawlerTask
		q := query().WithContext(ctx).Order("id").Limit(batchSize)
		if lastID > 0 {
			q = q.Where("id > ?", lastID)
		}
		if err := q.Find(&tasks).Error; err != nil {
			if ctx.Err() != nil {
				return total, nil
			}
			return total, err
		}
		if len(tasks) == 0 {
			return total, nil
		}
		total += processConcurrently(ctx, tasks, workers, requeue)
		lastID = tasks[len(tasks)-1].ID
		if len(tasks) < batchSize {
			return total, nil // 最后一批
		}
		// 批间停顿：只有"可能还有下一批"时才停（最后一批上面已经 return）。
		// 停机信号能立刻打断 —— Engine.Stop 第一步就是 cancel，这条协程在 tickerWG 里等着，
		// 不能被 30m 的停顿卡住优雅关停。
		if !e.sleepBatchPause(pause) {
			return total, nil
		}
	}
}

// sleepBatchPause 批间停顿；返回 false 表示停机信号已到，调用方应停止继续分批（不算错误）。
func (e *Engine) sleepBatchPause(pause time.Duration) bool {
	if e.queueContext().Err() != nil {
		return false
	}
	if pause <= 0 {
		return true
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	if e.ctx == nil {
		// 单测里可能直接构造 Engine：没有生命周期就退化成纯等待。
		<-timer.C
		return true
	}
	select {
	case <-e.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// queueContext also supports declaration-only test engines without a lifecycle.
func (e *Engine) queueContext() context.Context {
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// Cancellation stops new work; admitted callbacks still finish before returning.
func processConcurrently(ctx context.Context, tasks []models.CrawlerTask, workers int, requeue func(*models.CrawlerTask) bool) int {
	jobs := make(chan models.CrawlerTask, len(tasks))
	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)

	var wg sync.WaitGroup
	var processed int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if ctx.Err() != nil {
					return
				}
				if requeue(&t) {
					atomic.AddInt64(&processed, 1)
				}
			}
		}()
	}
	wg.Wait()
	return int(processed)
}
