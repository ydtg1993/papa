package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// processInBatches 分页查询并并发处理任务，避免一次性全量加载到内存（海量失败/卡死任务时防内存尖峰）。
// query 返回带条件的查询（不含 Order/Limit）；requeue 处理单条并返回是否成功；
//
// 分页游标是**自己上一批的最大 id**（keyset），不是 OFFSET、也不能指望「处理过的行会离开结果集」。
// 后者是原来的写法，它一直没出事只是因为 recover_queue 那条 `updated_at < cutoff` 顺手把行
// 踢出了结果集 —— 而启动恢复把 processing 改成 pending 后，行**依然满足**「未到终态」，
// 再 `Limit` 查一遍就是原地打转、永远跑不完（而且它持着队列锁 + 手动触发的 HTTP 请求）。
// 改成 keyset 之后三个队列都是「每条最多看一次」：跑不动的那些（阶段没注册之类）跳过就跳过，不成环。
func (e *Engine) processInBatches(query func() *gorm.DB, batchSize, workers int, requeue func(*models.CrawlerTask) bool) (int, error) {
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
