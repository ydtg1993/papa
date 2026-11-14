package engine

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/models"
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
// 治理队列现在统一走 markRequeueFailed。
func TestRequeuePersistsPendingBeforeRequestingRecovery(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)
	e.errorQueue = &errorQueueState{}
	row := &models.CrawlerTask{ID: 7, Site: "stub", Stage: "stub", URL: "https://example.com"}
	if !e.requeueFailedTask()(row) {
		t.Fatal("durable reset rejected")
	}
	if strings.Contains(f.written(), "CONCAT") || len(e.deliveryState().Notifications(stubKey)) != 1 {
		t.Fatal("successful reset must request recovery rather than mark failure")
	}
}

func TestMarkRequeueFailedSkipsAlreadyFailed(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	sql := f.written() // 空
	e.markRequeueFailed(recoverQueueName, 7, errNotSubmitted)
	if !strings.Contains(f.written(), "status <>") {
		t.Fatalf("守卫应写在语句里：\n%s", f.written())
	}
	_ = sql
}

// 分页必须靠自己的 keyset 游标推进，**不能**指望「处理过的行会离开结果集」。
//
// 回归点：启动恢复把 processing 改成 pending，那条依然满足「未到终态」——
// 原来那句 `query().Order("id").Limit(n)` 会一遍遍查回同一批，永远跑不完（启动恢复再也回不来）。
// 它一直没出事，只是因为旧 query 里那句 `updated_at < cutoff` 顺手把行踢出了结果集。
//
// 这里让假库返回 3 行、处理函数不改变行的匹配性（模拟"结果集不缩小"），
// 断言每条只被处理一次、且从第二轮起带上了 `id > ?`。
// maxQueries 是兜底：真死循环时要**失败**，不是挂住（假库会报 SELECT 超限）。
func TestProcessInBatchesUsesKeysetCursor(t *testing.T) {
	f := newFakeTaskDB()
	f.rows = []fakeRow{f.rowWithID(1), f.rowWithID(2), f.rowWithID(3)}
	f.maxQueries = 20
	e, _ := urgentEngine(t, f)

	var mu sync.Mutex
	seen := make(map[uint]int)
	n, err := e.processInBatches(e.recoverQueueQuery(), 1, 1, 0, func(task *models.CrawlerTask) bool {
		mu.Lock()
		seen[task.ID]++
		mu.Unlock()
		return true // 「处理成功」但不让行离开结果集
	})
	if err != nil {
		t.Fatalf("processInBatches = %v\nSQL:\n%s", err, f.readSQL())
	}
	if n != 3 {
		t.Fatalf("processed = %d, want 3", n)
	}
	for _, id := range []uint{1, 2, 3} {
		if seen[id] != 1 {
			t.Fatalf("任务 %d 被处理 %d 次，want 1（seen=%v）", id, seen[id], seen)
		}
	}
	// 3 批有行 + 1 批空 = 4 次 SELECT。少于 4 说明 LIMIT 没被假库照做、分页没被走到。
	if got := len(f.queries); got != 4 {
		t.Fatalf("SELECT 次数 = %d, want 4（3 批有行 + 1 批空）：\n%s", got, f.readSQL())
	}
	if !strings.Contains(f.readSQL(), "id > ?") {
		t.Fatalf("第二轮起应带 keyset 游标 `id > ?`：\n%s", f.readSQL())
	}
}
