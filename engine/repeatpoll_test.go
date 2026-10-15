package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
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
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.pools.get("stub:stub").Start(e.ctx, func(context.Context, *Task) error { return nil })
	e.cfg.RepeatQueue = config.RepeatQueueConfig{Enabled: true, Interval: 20 * time.Millisecond}
	e.startRepeatQueue()

	time.Sleep(80 * time.Millisecond) // 让它至少跑过一轮
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("Stop 应当把 ticker 也算进「排空」（它已经随 ctx 退出）")
	}
	before := f.written()
	time.Sleep(100 * time.Millisecond)
	if after := f.written(); after != before {
		t.Fatalf("Stop 返回后不该再有人写库：\n新增：\n%s", strings.TrimPrefix(after, before))
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
		return soonestRepeatQueryOn(tx).Find(&rows)
	})
	for _, want := range []string{"MIN(CAST(CASE", "AS SIGNED", "AS epoch", "UNIX_TIMESTAMP"} {
		if !strings.Contains(sql, want) {
			t.Errorf("「最早到点」查询里缺少 %q ｜ %s", want, sql)
		}
	}
	// 反向：聚合外面必须是 CAST，不能又变回裸的 MIN(CASE ...)
	if strings.Contains(sql, "MIN(CASE") {
		t.Errorf("CAST 被去掉了（MIN(CASE ...) 的 DECIMAL 扫不进 *int64）｜ %s", sql)
	}
}

// 「最早到点」那条聚合的底必须是**只按 repeatable** 筛，不能套终态（status IN）条件。
// 否则 pending/processing 行的未来 next_repeat_at 进不了聚合，节拍退回全局 interval
// （见 soonestRepeatQueryOn 的注释：状态取舍交给 CASE，活跃行只看未来 next_repeat_at）。
func TestSoonestRepeatQueryIncludesActiveRows(t *testing.T) {
	db := dryRunMySQL(t)
	sql := renderSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		var rows []struct{ Epoch *int64 }
		return soonestRepeatQueryOn(tx).Find(&rows)
	})
	if !strings.Contains(sql, "WHERE repeatable = ?") {
		t.Errorf("WHERE 应只按 repeatable 筛：%s", sql)
	}
	if strings.Contains(sql, "repeatable = ? AND status IN") {
		t.Errorf("WHERE 不应再把 status IN 套进去（会把活跃行排除掉）：%s", sql)
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
