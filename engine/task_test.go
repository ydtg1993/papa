package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/datatypes"
)

func TestTaskUnique(t *testing.T) {
	task := &Task{URL: "https://example.com", Stage: "mock"}
	if got := task.Unique(); got != "mock|https://example.com" {
		t.Fatalf("default unique = %q", got)
	}
	task.IdempotencyKey = "catalog:cat:2"
	if got := task.Unique(); got != "catalog:cat:2" {
		t.Fatalf("idempotency key unique = %q", got)
	}
}

func TestTaskDeliverAt(t *testing.T) {
	task := &Task{}
	if !task.deliverAt().IsZero() {
		t.Fatal("no delay should be zero time")
	}

	task.Delay = time.Minute
	if task.deliverAt().IsZero() {
		t.Fatal("Delay should produce a non-zero deliver time")
	}

	// NotBefore 优先于 Delay
	at := time.Now().Add(2 * time.Minute)
	task.NotBefore = at
	if !task.deliverAt().Equal(at) {
		t.Fatal("NotBefore should take precedence over Delay")
	}
}

// UpdateStatus 只能动 status / error 两列。
//
// 回归点：原来是「SELECT 整行 → 改字段 → Save 整行」，会把读到的 content 旧快照盖回库里 ——
// 业务刚 SaveResult 写进去的内容就这么没了（两条并发的路互相覆盖）。
func TestUpdateStatusOnlyTouchesItsOwnColumns(t *testing.T) {
	f := newFakeTaskDB()
	db := openFakeTaskDB(t, f)
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}

	if !task.UpdateStatus(db, models.TaskStatusSuccess, nil) {
		t.Fatal("正常路径应返回 true")
	}
	if !task.UpdateStatus(db, models.TaskStatusFailed, errors.New("boom")) {
		t.Fatal("失败路径应返回 true")
	}

	got := f.written()
	if !strings.Contains(got, "`status`") {
		t.Fatalf("应写入 status：\n%s", got)
	}
	for _, never := range []string{"`content`", "`title`", "`url`", "`stage`", "`idempotency_key`", "`retry`"} {
		if strings.Contains(got, never) {
			t.Fatalf("不该碰 %s（整行写会盖掉业务刚写的 content）：\n%s", never, got)
		}
	}
	if strings.Contains(got, "SELECT") {
		t.Fatalf("不该先把整行读回来：\n%s", got)
	}
	if !strings.Contains(got, "CONCAT") {
		t.Fatalf("失败时应在 SQL 里追加 error，而不是读出来拼字符串：\n%s", got)
	}
	if !f.writtenArgs_hasError("boom") {
		t.Fatalf("追加的错误内容应作为参数传给 SQL：%s", f.writtenArgs())
	}
}

/* ---------- Meta：任务身份随行落库 ---------- */

// 提交时带的业务键必须落库。
//
// 它是任务身份的一半 —— `URL` 只说明"抓哪个地址"，不说明"这条任务属于哪条业务行"。
// 而框架推荐（FETCHER_WRITING_GUIDE 1.2）用 Meta 承载业务键、明确要求别把它拼进 URL。
func TestSubmitTaskPersistsMeta(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务：走 INSERT
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))

	task := &Task{
		Stage: "stub", URL: "https://example.com/1",
		Meta: map[string]string{"series_id": "119002", "episode_id": "3"},
	}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}

	writes := f.written()
	if !strings.Contains(writes, "`meta`") {
		t.Fatalf("INSERT 应带上 meta 列：\n%s", writes)
	}
	args := f.writtenArgs()
	for _, want := range []string{`"series_id":"119002"`, `"episode_id":"3"`} {
		if !strings.Contains(args, want) {
			t.Fatalf("meta 应序列化成 JSON 落库，参数里缺少 %s：%s", want, args)
		}
	}
}

// 五条「从行重建 Task」的路，一条都不能丢 Meta：
// 恢复队列（进程重启时会走）、轮询队列、错误队列、后台「重投」、后台「加急」。
//
// 漏掉任何一条的后果都一样：库里 URL、幂等键都在，唯独业务键没了 ——
// handler 报的是一句莫名其妙的"缺少 series_id"，且这类结构错误通常被包成不可重试，
// 一次就判死，人工点「重投」也走同一条路，救不回来。
func TestRowRebuildKeepsMeta(t *testing.T) {
	const metaJSON = `{"series_id":"119002","episode_id":"3"}`

	cases := []struct {
		name string
		run  func(e *Engine, row *models.CrawlerTask) error
	}{
		{"恢复队列", func(e *Engine, row *models.CrawlerTask) error {
			if !e.requeueRecoverTask(row) {
				return errors.New("requeueRecoverTask 返回 false")
			}
			return nil
		}},
		{"轮询队列", func(e *Engine, row *models.CrawlerTask) error {
			if !e.requeueRepeatTask(row) {
				return errors.New("requeueRepeatTask 返回 false")
			}
			return nil
		}},
		{"错误队列", func(e *Engine, row *models.CrawlerTask) error {
			if !e.requeueFailedTask(row) {
				return errors.New("requeueFailedTask 返回 false")
			}
			return nil
		}},
		// 后台那两个是引擎自己的入口：行由 loadTask 从库里读出来，同样要带 Meta
		{"后台重投", func(e *Engine, _ *models.CrawlerTask) error { return e.RetryTask(7, 0) }},
		{"后台加急", func(e *Engine, _ *models.CrawlerTask) error { return e.UrgentTask(7) }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeTaskDB()
			f.row["meta"] = []byte(metaJSON) // 库里这列有值
			pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
			e := queueEngine(t, f, pool)

			delivered := make(chan *Task, 4)
			pool.Start(context.Background(), func(_ context.Context, task *Task) error {
				delivered <- task
				return nil
			})
			t.Cleanup(func() { pool.Stop(time.Second) })

			row := &models.CrawlerTask{
				ID: 7, Stage: "stub", URL: "https://example.com",
				Status: models.TaskStatusPending, Repeatable: models.RepeatableYes,
				Meta: datatypes.JSON(metaJSON),
			}
			if err := c.run(e, row); err != nil {
				t.Fatalf("%s：%v", c.name, err)
			}

			select {
			case task := <-delivered:
				if task.Meta["series_id"] != "119002" || task.Meta["episode_id"] != "3" {
					t.Fatalf("重建出来的任务丢了业务键：Meta = %v", task.Meta)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("任务没进队列")
			}
		})
	}
}

// meta 列坏了（人工改库、或半截写入）不该让这条任务彻底没人管：按无 Meta 继续投递。
//
// 这一列只有 toModel 一处写，坏值只可能来自框架之外；此时的取舍是"照样跑"而不是"拒绝重投" ——
// 拒绝的话任务会永远卡在待处理，比带着空 Meta 跑一次更糟（后者至少还能在日志/trace 里看见）。
func TestRowRebuildToleratesBrokenMeta(t *testing.T) {
	const broken = `{"series_id":`

	f := newFakeTaskDB()
	f.row["meta"] = []byte(broken)
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := queueEngine(t, f, pool)

	delivered := make(chan *Task, 4)
	pool.Start(context.Background(), func(_ context.Context, task *Task) error {
		delivered <- task
		return nil
	})
	t.Cleanup(func() { pool.Stop(time.Second) })

	row := &models.CrawlerTask{
		ID: 7, Stage: "stub", URL: "https://example.com",
		Status: models.TaskStatusPending, Meta: datatypes.JSON(broken),
	}
	if !e.requeueRecoverTask(row) {
		t.Fatal("坏 meta 不该让这条任务被跳过")
	}

	select {
	case task := <-delivered:
		if task.Meta != nil {
			t.Fatalf("坏值应解成无 Meta，实得 %v", task.Meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("任务没进队列")
	}
}

// 站点归属随行落库，也在重建路径上还原 —— 和 Meta 同一套机制（P0 的 taskFromRecord 是唯一入口）。
//
// 站点由**目标阶段**决定：调用方投任务时不用自己写 site，引擎按"这个阶段属于哪个站"补上；
// 于是跨站派发（A 站的 catalog 投给 B 站的 detail）自然落到 B 站，不需要业务传参。
func TestTaskSiteIsPersistedAndRestored(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务：走 INSERT
	e := submitEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.stages["stub"] = &stageInfo{workerPool: e.stages["stub"].workerPool, config: StageConfig{Site: "huangguo"}}

	task := &Task{Stage: "stub", URL: "https://example.com/1"}
	if err := e.SubmitTask(task); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if task.Site != "huangguo" {
		t.Fatalf("站点应当由目标阶段补上，实得 %q", task.Site)
	}
	if !strings.Contains(f.writtenArgs(), "huangguo") {
		t.Fatalf("站点应随任务落库：%s", f.writtenArgs())
	}

	// 重建（恢复/重投/后台动作都走这一条）要把站点带回来
	got := e.taskFromRecord(&models.CrawlerTask{ID: 7, Stage: "stub", Site: "huangguo"})
	if got.Site != "huangguo" {
		t.Fatalf("重建出来的任务丢了站点：%q", got.Site)
	}
}
