package crawler

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v2/models"
)

var errNotSubmitted = errors.New("boom")

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

// 重新投递失败不能把行留在 pending。
//
// 回归点：error_queue 先把行重置成 pending 再提交，提交失败只 log 不管 —— 而本队列只捞
// status=failed，于是这条任务再也不会被任何队列捞起来：运营看着是"排队中"，实际永远不会执行。
// 三个治理队列现在统一走 markRequeueFailed。
func TestRequeueFailureMarksTaskFailed(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)
	pool.Stop(0) // 停掉的池：Submit 必然失败，且不是 ErrQueueFull（不会走溢出那条路）

	row := &models.CrawlerTask{ID: 7, Stage: "stub", URL: "https://example.com"}
	if e.requeueFailedTask(row) {
		t.Fatal("投递必然失败，应返回 false")
	}

	got := f.written()
	if !strings.Contains(got, "`status`") || !strings.Contains(got, "CONCAT") {
		t.Fatalf("应把行标成 failed 并追加原因：\n%s", got)
	}
	if !strings.Contains(f.writtenArgs(), "error_queue") {
		t.Fatalf("原因里应带上是哪个队列写的：%s", f.writtenArgs())
	}
}

// 已经是 failed 的行不再重复写：走 SubmitTask 那条路失败时 submitToPool 已经标过，
// 再标一次只会在 error 列里多贴一行重复的原因。
func TestMarkRequeueFailedSkipsAlreadyFailed(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	sql := f.written() // 空
	e.markRequeueFailed(QueueRecover, 7, errNotSubmitted)
	if !strings.Contains(f.written(), "status <>") {
		t.Fatalf("守卫应写在语句里：\n%s", f.written())
	}
	_ = sql
}
