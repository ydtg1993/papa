package crawler

import (
	"sync"
	"sync/atomic"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// markRequeueFailed 把「重新投递失败」写回行上：标 failed、retry+1、追加原因。
//
// 三个治理队列（error / recover / repeat）共用它。之前 error_queue 和 repeat_queue 在这条路上
// 只 log 不管，行停在 pending —— 而它们的捞取条件是 status=failed（error_queue）与
// status IN (success, failed)（repeat_queue），于是这条任务再也不会被任何一个队列捞起来：
// 运营看着是"排队中"，实际早就没人会执行它了。
//
// 带 `status <> failed` 是有意的：走 SubmitTask 那条路失败时，submitToPool 已经标过 failed
// 并把原始错误写进 error 列了，这里再标一次只会在 error 列里多贴一行重复的原因。已经是 failed 就跳过。
func (e *Engine) markRequeueFailed(queue string, id uint, err error) {
	res := e.db.Model(&models.CrawlerTask{}).
		Where("id = ? AND status <> ?", id, models.TaskStatusFailed).
		Updates(map[string]any{
			"status": models.TaskStatusFailed,
			"retry":  gorm.Expr("retry + 1"),
			"error":  gorm.Expr("CONCAT(COALESCE(error, ''), ?)", queue+" 重新投递失败："+err.Error()+"\n"),
		})
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("%s: mark task %d failed: %s", queue, id, res.Error.Error())
	}
}

// processInBatches 分页查询并并发处理任务，避免一次性全量加载到内存（海量失败/卡死任务时防内存尖峰）。
// query 返回带条件的查询（不含 Order/Limit）；requeue 处理单条并返回是否成功。
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
	for {
		var tasks []models.CrawlerTask
		q := query().Order("id").Limit(batchSize)
		if lastID > 0 {
			q = q.Where("id > ?", lastID)
		}
		if err := q.Find(&tasks).Error; err != nil {
			return total, err
		}
		if len(tasks) == 0 {
			return total, nil
		}
		total += processConcurrently(tasks, workers, requeue)
		lastID = tasks[len(tasks)-1].ID
		if len(tasks) < batchSize {
			return total, nil // 最后一批
		}
	}
}

// processConcurrently 用 worker 并发处理一批任务，返回成功数。
func processConcurrently(tasks []models.CrawlerTask, workers int, requeue func(*models.CrawlerTask) bool) int {
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
				if requeue(&t) {
					atomic.AddInt64(&processed, 1)
				}
			}
		}()
	}
	wg.Wait()
	return int(processed)
}
