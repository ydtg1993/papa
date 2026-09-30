package crawler

import (
	"sync"
	"sync/atomic"

	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// processInBatches 分页查询并并发处理任务，避免一次性全量加载到内存（海量失败/卡死任务时防内存尖峰）。
// query 返回带条件的查询（不含 Order/Limit）；requeue 处理单条并返回是否成功。
func (e *Engine) processInBatches(query func() *gorm.DB, batchSize, workers int, requeue func(*models.CrawlerTask) bool) (int, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	if workers <= 0 {
		workers = 1
	}
	var total int
	for {
		var tasks []models.CrawlerTask
		if err := query().Order("id").Limit(batchSize).Find(&tasks).Error; err != nil {
			return total, err
		}
		if len(tasks) == 0 {
			return total, nil
		}
		total += processConcurrently(tasks, workers, requeue)
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
