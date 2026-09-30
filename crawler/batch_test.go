package crawler

import (
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v2/models"
)

func TestProcessConcurrently(t *testing.T) {
	tasks := make([]models.CrawlerTask, 25)
	for i := range tasks {
		tasks[i] = models.CrawlerTask{ID: uint(i + 1)}
	}

	var mu sync.Mutex
	seen := make(map[uint]bool)

	n := processConcurrently(tasks, 4, func(t *models.CrawlerTask) bool {
		mu.Lock()
		seen[t.ID] = true
		mu.Unlock()
		// 奇数成功、偶数失败，验证计数只算成功
		return t.ID%2 == 1
	})

	if n != 13 {
		t.Fatalf("processed = %d, want 13（奇数成功）", n)
	}
	if len(seen) != 25 {
		t.Fatalf("seen = %d, want 25（所有任务都应被处理）", len(seen))
	}
}
