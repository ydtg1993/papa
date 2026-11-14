package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
)

// 到点那条查询带可轮询、已完成、到点条件；「轮询任务」（全量）只差"不看周期"这一条。
// 假库不解析 WHERE，所以只能钉 SQL 形态（真过滤在 MySQL 那边）。
func TestRepeatQueryShape(t *testing.T) {
	cases := []struct {
		name  string
		run   func(*Engine)
		want  []string
		never []string
	}{
		{
			"到点：可轮询 + 已完成 + 到点",
			func(e *Engine) { e.repeatQueueQuery()().Find(&[]models.CrawlerTask{}) },
			[]string{"repeatable = ?", "status IN", "next_repeat_at"},
			nil,
		},
		{
			"全量：可轮询 + 已完成（**不看周期**）",
			func(e *Engine) { e.repeatForceQueueQuery()().Find(&[]models.CrawlerTask{}) },
			[]string{"repeatable = ?", "status IN"},
			[]string{"next_repeat_at"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
			tc.run(e)
			read := f.readSQL()
			for _, w := range tc.want {
				if !strings.Contains(read, w) {
					t.Fatalf("SQL 里缺少 %q：\n%s", w, read)
				}
			}
			for _, n := range tc.never {
				if strings.Contains(read, n) {
					t.Fatalf("SQL 里不该出现 %q：\n%s", n, read)
				}
			}
		})
	}
}

// Stop 要等在途的那一跳：不等的话调用方看到 drained=true 就去关库，
// ticker 里正在写的语句会直接报 `sql: database is closed`。
func TestStopWaitsForTickers(t *testing.T) {
	f := newFakeTaskDB()
	// Fake WHERE clauses do not filter the default pending row. Model the
	// conditional repeat reset losing its CAS, so no task should be delivered.
	f.affected = 0
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var enterOnce sync.Once
	f.onExec = func(int) {
		// The competing update removes this row from subsequent repeat scans.
		// onExec runs under the fake DB mutex, so this change is synchronized.
		f.noRows = true
		enterOnce.Do(func() { close(entered) })
		<-release
	}
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.pools.Get("stub:stub").Start(e.ctx, func(context.Context, *Task) error { return nil })
	e.cfg.RepeatQueue = config.RepeatQueueConfig{Enabled: true, Interval: time.Millisecond}
	e.startRepeatQueue()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("repeat ticker did not enter SQL write")
	}
	// The write is certainly in flight: cancellation alone cannot mean drained.
	if drained, stats := e.Stop(0); drained || len(stats) != 0 {
		t.Fatalf("blocked ticker must prevent drain without inventing queued tasks: drained=%v stats=%+v", drained, stats)
	}
	unblock()
	if drained, stats := e.Stop(time.Second); !drained || len(stats) != 0 {
		t.Fatalf("completed ticker with failed repeat CAS should drain: drained=%v stats=%+v", drained, stats)
	}
	// Wait-group completion is the evidence of no remaining SQL producer; no
	// arbitrary post-Stop sleep is needed to guess whether another tick will fire.
	e.tickerWG.Wait()
	if got := strings.Count(f.written(), "UPDATE"); got != 1 {
		t.Fatalf("expected exactly one in-flight repeat reset, got %d: %s", got, f.written())
	}
	if e.delays.Len() != 0 || e.overflow.Count() != 0 {
		t.Fatalf("failed repeat CAS retained work: delayed=%d spill=%d", e.delays.Len(), e.overflow.Count())
	}
}

// TestSoonestRepeatQueryCastsToSignedInt 「最早到点」那条聚合必须把 MySQL 的 DECIMAL 转成整数。
//
// 这条测试的来历是真库上的一件事（原话照抄）：
//
//	sql: Scan error on column index 0, name "epoch": converting driver.Value type
//	[]uint8 ("1791705102.000") to a int64: invalid syntax
//
// 因为 next_repeat_at 是 datetime(3)，而 MySQL 的 UNIX_TIMESTAMP(datetime(3)) 返回 **DECIMAL**
// （带小数位）。后果不是"慢一点"：soonestRepeatIn 把 err 吞成 ok=false → repeatTickInterval
// 回退成配置里那句"最粗兜底"的周期（2h）→ 自动轮询永远按配置周期跑，任务自己定的短周期、
// 以及"刚开轮询 / 刚改周期"的唤醒全都不生效（后台还显示 enabled=true，看不出坏）。
//
// 假库不解析 SQL、DryRun 只拼不执行，所以这里只能钉住"那层 CAST 还在"（值层面由真库跑一次证）——
// 但恰恰是"有人觉得 CAST 多余顺手删掉"这一种改法能被它拦住。
func TestSoonestRepeatQueryCastsToSignedInt(t *testing.T) {
	db := dryRunMySQL(t)
	// 用 ToSQL 把语句拼出来（DryRun 下 Scan 会报 "dry run mode unsupported"，Find 这条能出 SQL）
	var rows []struct{ Epoch *int64 }
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return (&Engine{}).soonestRepeatQueryOn(tx).Find(&rows)
	})
	for _, want := range []string{"MIN(CAST(UNIX_TIMESTAMP", "AS SIGNED", "AS epoch", "UNIX_TIMESTAMP"} {
		if !strings.Contains(sql, want) {
			t.Errorf("「最早到点」查询里缺少 %q ｜ %s", want, sql)
		}
	}
	if strings.Count(sql, "CAST(UNIX_TIMESTAMP") != 1 {
		t.Fatalf("candidate integer casts missing: %s", sql)
	}
	// 反向：聚合外面必须是 CAST，不能又变回裸的 MIN(CASE ...)
	if strings.Contains(sql, "MIN(CASE") {
		t.Errorf("CAST 被去掉了（MIN(CASE ...) 的 DECIMAL 扫不进 *int64）｜ %s", sql)
	}
}

// Future rows include active and unknown/NULL statuses. Filtering them out
// would change scheduling even though they are not currently due for submission.
func TestSoonestRepeatQueryIncludesActiveRows(t *testing.T) {
	db := dryRunMySQL(t)
	var rows []struct{ Epoch *int64 }
	q := (&Engine{}).soonestRepeatQueryOn(db).Find(&rows)
	sql := q.Statement.SQL.String()
	for _, fragment := range []string{"status IN (?,?) OR next_repeat_at > NOW()", "COALESCE(next_repeat_at, NOW())", "MIN(CAST(UNIX_TIMESTAMP"} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("missing %q: %s", fragment, sql)
		}
	}
	if strings.Contains(sql, "UNION") || strings.Contains(sql, "LIMIT") {
		t.Fatalf("unexpected branch shape: %s", sql)
	}
	if strings.Contains(sql, "FOR UPDATE") || strings.Contains(sql, ";") {
		t.Fatal("query must be one non-locking statement")
	}
}

// finishTask 两条分支：非轮询清去重键，轮询留在去重表并唤醒轮询队列重算节拍。
func TestFinishTaskWakesRepeatQueueForRepeatable(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	plain := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com/plain"}
	e.dedupCache.Add(plain.Unique())
	e.finishTask(plain)
	if e.dedupCache.Get(plain.Unique()) {
		t.Fatal("非轮询任务落终态应清掉去重键")
	}

	repeat := &Task{ID: 8, Stage: "stub", Site: "stub", URL: "https://example.com/repeat", Repeatable: true}
	e.dedupCache.Add(repeat.Unique())
	e.finishTask(repeat)
	if !e.dedupCache.Get(repeat.Unique()) {
		t.Fatal("轮询任务落终态应留在去重表，等 repeat_queue 重新捞")
	}
	select {
	case <-e.repeatQueue.wake:
	default:
		t.Fatal("轮询任务落终态应唤醒轮询队列重算节拍")
	}
}
