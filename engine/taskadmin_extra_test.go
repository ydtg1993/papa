package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/models"
)

// retryEngineWithNilPool 造一个「阶段注册了但没走 ApplyRegisterStage」的引擎：
// 这时 e.stages 里有它、workerPool 却是 nil。
func retryEngineWithNilPool(t *testing.T, f *fakeTaskDB) *Engine {
	t.Helper()
	e, _ := urgentEngine(t, f)
	e.stages["stub"] = &stageInfo{}
	return e
}

/* ---------- RetryTask ---------- */

// 重投的语义与 error_queue 的单行重投一致：状态归 pending、重试次数归零、代次 +1。
func TestRetryTaskHappyPath(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)

	if err := e.RetryTask(7, 0); err != nil {
		t.Fatalf("RetryTask = %v", err)
	}

	sql := f.written()
	if !strings.Contains(sql, "`reprocess`") {
		t.Fatalf("代次应 +1（它同时是防重复点击的版本号）：\n%s", sql)
	}
	if !strings.Contains(sql, "`retry`") || !strings.Contains(sql, "`status`") {
		t.Fatalf("应把状态与重试次数一起重置：\n%s", sql)
	}
	// 版本守卫写在语句里：WHERE reprocess = <旧值>
	if !strings.Contains(sql, "reprocess = ?") {
		t.Fatalf("应带上 reprocess 版本条件：\n%s", sql)
	}

	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("重投的任务应入队，实得 %d", main+urgent)
	}
	if !e.dedupCache.Get("stub|https://example.com") {
		t.Fatal("重投后应进内存去重表")
	}
}

func TestRetryTaskRejections(t *testing.T) {
	cases := []struct {
		name string
		prep func(*fakeTaskDB)
		run  func(*testing.T, *fakeTaskDB) *Engine
		want error
	}{
		{
			"行不存在",
			func(f *fakeTaskDB) { f.noRows = true },
			nil,
			ErrTaskNotFound,
		},
		{
			"阶段未注册",
			func(f *fakeTaskDB) { f.row["stage"] = "nope" },
			nil,
			ErrStageNotRegistered,
		},
		{
			"阶段注册了但池子没建",
			nil,
			retryEngineWithNilPool,
			ErrStageNotRegistered,
		},
		{
			"条件更新没匹配上 + 正在处理中",
			func(f *fakeTaskDB) {
				f.affected = 0
				f.row["status"] = int64(models.TaskStatusProcessing)
			},
			nil,
			ErrTaskProcessing,
		},
		{
			"条件更新没匹配上 + 版本号变了（别人先点过）",
			func(f *fakeTaskDB) { f.affected = 0 },
			nil,
			ErrTaskChanged,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTaskDB()
			if c.prep != nil {
				c.prep(f)
			}
			var e *Engine
			if c.run != nil {
				e = c.run(t, f)
			} else {
				e, _ = urgentEngine(t, f)
			}
			if err := e.RetryTask(7, 0); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// 重投时队列已满 → 溢出到 DB 由 drain 回灌，**不算失败**（对后台表现为操作成功）。
func TestRetryTaskSpillsWhenQueueFull(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 4, 0.25) // 水位 = 1 条
	e, _ := urgentEngine(t, f)
	e.stages["stub"] = &stageInfo{workerPool: pool}
	e.spilled = make(map[string][]*Task) // NewEngine 会建，手搓的 Engine 得自己补

	if err := pool.Submit(&Task{URL: "occupy", Stage: "stub"}); err != nil {
		t.Fatal(err)
	}
	if err := e.RetryTask(7, 0); err != nil {
		t.Fatalf("队列满时应溢出而不是报错：%v", err)
	}
	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("应溢出 1 条，实得 %d", got)
	}
	if got := e.spilledCount.Load(); got != 1 {
		t.Fatalf("溢出计数 = %d, want 1", got)
	}
}

// 投递真失败（池子已停机）：报错并把 key 从去重表里撤掉，让下次还能投。
func TestRetryTaskReportsSubmitFailure(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	pool.Stop(0)
	e, _ := urgentEngine(t, f)
	e.stages["stub"] = &stageInfo{workerPool: pool}
	e.spilled = make(map[string][]*Task) // NewEngine 会建，手搓的 Engine 得自己补

	err := e.RetryTask(7, 0)
	if err == nil {
		t.Fatal("投递失败应当报错")
	}
	if !strings.Contains(err.Error(), "submit task 7") {
		t.Fatalf("错误信息应带上任务 ID：%v", err)
	}
	if e.dedupCache.Get("stub|https://example.com") {
		t.Fatal("投递失败应把 key 从去重表里撤掉")
	}
}

/* ---------- MarkTaskFailed ---------- */

func TestMarkTaskFailedHappyPath(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	if err := e.MarkTaskFailed(7, "内容违规"); err != nil {
		t.Fatalf("MarkTaskFailed = %v", err)
	}
	sql := f.written()
	if !strings.Contains(sql, "CONCAT") {
		t.Fatalf("原因应在 SQL 里追加到 error 列（而不是读出来拼字符串）：\n%s", sql)
	}
	if !strings.Contains(sql, "status IN") {
		t.Fatalf("应只碰非终态的行：\n%s", sql)
	}
	args := f.writtenArgs()
	if !strings.Contains(args, "内容违规") {
		t.Fatalf("原因没传进 SQL：%s", args)
	}
	// 不给原因时也要带上一句说明，别在 error 列里留个光秃秃的冒号
	f2 := newFakeTaskDB()
	e2, _ := urgentEngine(t, f2)
	if err := e2.MarkTaskFailed(7, ""); err != nil {
		t.Fatalf("MarkTaskFailed(空原因) = %v", err)
	}
	if got := f2.writtenArgs(); !strings.Contains(got, "后台手动标记失败") || strings.Contains(got, "：\n") {
		t.Fatalf("空原因时的文案不对：%q", got)
	}
}

func TestMarkTaskFailedRejections(t *testing.T) {
	t.Run("已到终态", func(t *testing.T) {
		f := newFakeTaskDB()
		f.affected = 0
		f.row["status"] = int64(models.TaskStatusSuccess)
		e, _ := urgentEngine(t, f)

		if err := e.MarkTaskFailed(7, "r"); !errors.Is(err, ErrTaskTerminal) {
			t.Fatalf("err = %v, want ErrTaskTerminal", err)
		}
	})

	t.Run("行不存在", func(t *testing.T) {
		f := newFakeTaskDB()
		f.affected = 0
		f.noRows = true
		e, _ := urgentEngine(t, f)

		if err := e.MarkTaskFailed(7, "r"); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("err = %v, want ErrTaskNotFound", err)
		}
	})
}

/* ---------- DeleteTask ---------- */

// 删除要连内存去重表的残留一起清掉，否则同样的 URL 再也投不进来
// （内存说"已存在"，库里那行却没了）。
func TestDeleteTaskClearsDedupCache(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)
	e.dedupCache.Add("stub|https://example.com")

	if err := e.DeleteTask(7); err != nil {
		t.Fatalf("DeleteTask = %v", err)
	}
	if e.dedupCache.Get("stub|https://example.com") {
		t.Fatal("删除后应清掉内存去重表里的残留")
	}
	if len(e.dedupCache.entries) != 0 {
		t.Fatalf("去重表应为空：%+v", e.dedupCache.entries)
	}
}

func TestDeleteTaskRejections(t *testing.T) {
	t.Run("处理中的行拒绝删除", func(t *testing.T) {
		f := newFakeTaskDB()
		f.row["status"] = int64(models.TaskStatusProcessing)
		e, _ := urgentEngine(t, f)

		if err := e.DeleteTask(7); !errors.Is(err, ErrTaskProcessing) {
			t.Fatalf("err = %v, want ErrTaskProcessing", err)
		}
		if got := f.written(); got != "" {
			t.Fatalf("被拒时不该发删除语句：\n%s", got)
		}
	})

	t.Run("行不存在", func(t *testing.T) {
		f := newFakeTaskDB()
		f.noRows = true
		e, _ := urgentEngine(t, f)

		if err := e.DeleteTask(7); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("err = %v, want ErrTaskNotFound", err)
		}
	})

	t.Run("条件更新没匹配上（快照之后被 worker 取走了）", func(t *testing.T) {
		f := newFakeTaskDB()
		f.affected = 0
		e, _ := urgentEngine(t, f)

		if err := e.DeleteTask(7); !errors.Is(err, ErrTaskProcessing) {
			t.Fatalf("err = %v, want ErrTaskProcessing", err)
		}
	})
}

// 删除语句本身要带 `status <> processing` 守卫：光靠前面那次读是"先查再删"，
// 快照与删除之间 worker 完全可能认领走这一行。
func TestDeleteScopeCarriesGuard(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)

	if err := e.DeleteTask(7); err != nil {
		t.Fatalf("DeleteTask = %v", err)
	}
	if !strings.Contains(f.written(), "status <> ?") {
		t.Fatalf("删除应把守卫写进语句：\n%s", f.written())
	}
}

/* ---------- 后台操作的整体性质 ---------- */

// 被拒的后台操作不该改动任何行 —— 抽屉里点了报错、数据却变了，是最难查的一类问题。
//
// 这里只放**在写库之前就被拦下**的那几条。标失败/加急这类拦截是**条件更新**本身
// （守卫写在 WHERE 里），语句会发出去、只是匹配不到行 —— 那属于另一条测试。
func TestRejectedAdminOpsWriteNothing(t *testing.T) {
	ops := []struct {
		name string
		run  func(*Engine) error
		prep func(*fakeTaskDB)
	}{
		{"重投：行不存在", func(e *Engine) error { return e.RetryTask(7, 0) },
			func(f *fakeTaskDB) { f.noRows = true }},
		{"重投：阶段未注册", func(e *Engine) error { return e.RetryTask(7, 0) },
			func(f *fakeTaskDB) { f.row["stage"] = "nope" }},
		{"加急：已被取走", func(e *Engine) error { return e.UrgentTask(7) },
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusProcessing) }},
		{"加急：已结束", func(e *Engine) error { return e.UrgentTask(7) },
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusSuccess) }},
		{"删除：行不存在", func(e *Engine) error { return e.DeleteTask(7) },
			func(f *fakeTaskDB) { f.noRows = true }},
		{"删除：处理中的行", func(e *Engine) error { return e.DeleteTask(7) },
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusProcessing) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			f := newFakeTaskDB()
			op.prep(f)
			e, pool := urgentEngine(t, f)

			if err := op.run(e); err == nil {
				t.Fatal("应当被拒")
			}
			if got := f.written(); got != "" {
				t.Fatalf("被拒时不该写库：\n%s", got)
			}
			if main, urgent := pool.QueueDepths(); main+urgent != 0 {
				t.Fatal("被拒时不该入队")
			}
		})
	}
}

// 「标失败」对已终态的行：拦截靠 UPDATE 语句里的 `status IN (待处理, 处理中)`，
// 所以语句会发出去、只是 RowsAffected=0 —— 这里同时钉住"守卫在语句里"和"报错说得清原因"。
func TestMarkTaskFailedTerminalGuardIsInStatement(t *testing.T) {
	f := newFakeTaskDB()
	f.affected = 0
	f.row["status"] = int64(models.TaskStatusSuccess)
	e, pool := urgentEngine(t, f)

	if err := e.MarkTaskFailed(7, "r"); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("err = %v, want ErrTaskTerminal", err)
	}
	sql := f.written()
	if !strings.Contains(sql, "status IN") {
		t.Fatalf("守卫应写在语句里（不是先查再写）：\n%s", sql)
	}
	if main, urgent := pool.QueueDepths(); main+urgent != 0 {
		t.Fatal("标失败不该入队")
	}
}

// 加急时两条队列都满才溢出：快车道优先且满了会退到常规队列（best-effort 优先），
// 只有两条都塞不下才回 ErrQueueFull、由上层溢出到 DB。
func TestUrgentTaskSpillsWhenQueueFull(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 4, 0.25) // 常规水位 1 条、快车道水位 2 条
	e, _ := urgentEngine(t, f)
	e.stages["stub"] = &stageInfo{workerPool: pool}
	e.spilled = make(map[string][]*Task) // NewEngine 会建，手搓的 Engine 得自己补

	// 占满常规队列，再占满快车道
	if err := pool.Submit(&Task{URL: "occupy-main", Stage: "stub"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := pool.SubmitUrgent(&Task{URL: "occupy-urgent", Stage: "stub"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := e.UrgentTask(7); err != nil {
		t.Fatalf("两条队列都满时应溢出而不是报错：%v", err)
	}
	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("应溢出 1 条，实得 %d", got)
	}
	if got := e.spilledCount.Load(); got != 1 {
		t.Fatalf("溢出计数 = %d, want 1", got)
	}
}

// 加急投递真失败时报错并撤掉去重表里的 key。
func TestUrgentTaskReportsSubmitFailure(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	pool.Stop(0)
	e, _ := urgentEngine(t, f)
	e.stages["stub"] = &stageInfo{workerPool: pool}
	e.spilled = make(map[string][]*Task) // NewEngine 会建，手搓的 Engine 得自己补

	err := e.UrgentTask(7)
	if err == nil {
		t.Fatal("投递失败应当报错")
	}
	if !strings.Contains(err.Error(), "submit urgent task 7") {
		t.Fatalf("错误信息应带上任务 ID：%v", err)
	}
	if e.dedupCache.Get("stub|https://example.com") {
		t.Fatal("投递失败应把 key 从去重表里撤掉")
	}
}

// loadTask 把 gorm 的哨兵错误翻成自己的 ErrTaskNotFound：上层（admin/tasksource）
// 据此映射成 404，而不是把 "record not found" 原样回给浏览器。
func TestLoadTaskTranslatesNotFound(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e, _ := urgentEngine(t, f)

	if _, err := e.loadTask(7); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}

	f2 := newFakeTaskDB()
	e2, _ := urgentEngine(t, f2)
	got, err := e2.loadTask(7)
	if err != nil {
		t.Fatalf("loadTask = %v", err)
	}
	if got.ID != 7 || got.Stage != "stub" {
		t.Fatalf("载入的行不对：%+v", got)
	}
	_ = time.Second
}
