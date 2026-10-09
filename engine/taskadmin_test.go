package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// dryDB 造一个不会真的连库的 DB：DryRun + 跳过版本探测 + 关掉 Open 后的自动 Ping。
func dryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:3306)/x",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

// 并发保护的关键是「条件写在 UPDATE 语句本身」而不是先查再写。
// 这里断言的就是 production 里那几份 scope 函数（不是复制品）——
// RowsAffected 的分支需要真库，离线只能钉到语句这一层。
func TestAdminScopesCarryConditions(t *testing.T) {
	db := dryDB(t)

	cases := []struct {
		name string
		run  func() string
		want []string
	}{
		{
			"认领：只有待处理能被认领，并顺手把加急归零",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return claimScope(tx, 7).Updates(map[string]any{
						"status": models.TaskStatusProcessing,
						"urgent": false,
					})
				})
			},
			[]string{"UPDATE", "id = 7", "status = 0", "urgent"},
		},
		{
			// status 守卫不能少：否则 SELECT 与 UPDATE 之间被 claimTask 认领（它恰好把 urgent
			// 清成 0）时这里照样命中，把 urgent=1 留在一条已在跑、再没人清的行上。
			"加急：版本守卫 = 还是待处理 + 还没加急过",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return urgentScope(tx, 7).Update("urgent", true)
				})
			},
			[]string{"UPDATE", "id = 7", "status = 0", "urgent = false"},
		},
		{
			"重投：排除处理中 + reprocess 版本条件",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return retryScope(tx, 7, 3).Updates(map[string]any{
						"status":    models.TaskStatusPending,
						"reprocess": gorm.Expr("reprocess + 1"),
					})
				})
			},
			[]string{"UPDATE", "id = 7", "status <> 1", "reprocess = 3"},
		},
		{
			"标失败：只碰非终态，原因写进错误列",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return markFailedScope(tx, 7).Updates(map[string]any{
						"status": models.TaskStatusFailed,
						"error":  gorm.Expr("CONCAT(COALESCE(error, ''), ?)", "后台手动标记失败：内容违规\n"),
					})
				})
			},
			[]string{"UPDATE", "id = 7", "status IN", "CONCAT", "内容违规"},
		},
		{
			"删除：排除处理中",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return deleteScope(tx, 7).Delete(&models.CrawlerTask{})
				})
			},
			[]string{"DELETE", "id = 7", "status <> 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql := tc.run()
			for _, w := range tc.want {
				if !strings.Contains(sql, w) {
					t.Fatalf("SQL 里缺少 %q：\n%s", w, sql)
				}
			}
		})
	}
}

/* ---------- 后台「加急」的完整路径（假库返回结果集，覆盖取行→判状态→条件更新→投递） ---------- */

// urgentEngine 造一个接假库、带真 worker 池的引擎。
func urgentEngine(t *testing.T, f *fakeTaskDB) (*Engine, *workerpool.WorkerPool[*Task]) {
	t.Helper()
	db := openFakeTaskDB(t, f)
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	return &Engine{
		db:         db,
		loggerSet:  &loggers.LoggerSet{Engine: logrus.New(), DB: logrus.New()},
		cfg:        &config.Config{},
		stages:     map[string]*stageInfo{"stub": {workerPool: pool}},
		dedupCache: newDedupCache(0),
	}, pool
}

func TestUrgentTaskHappyPath(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)

	got := make(chan *Task, 1)
	pool.Start(context.Background(), func(_ context.Context, task *Task) error {
		got <- task
		return nil
	})
	defer pool.Stop(time.Second)

	if err := e.UrgentTask(7); err != nil {
		t.Fatalf("UrgentTask = %v, want nil", err)
	}

	// 1) 先落库标记加急 —— 后台「加急」列要是「是」，溢出/重启后也还认得出
	sql := f.written()
	if !strings.Contains(sql, "UPDATE `crawler_tasks`") || !strings.Contains(sql, "`urgent`") {
		t.Fatalf("应先条件更新 urgent 列，实得：\n%s", sql)
	}
	// 版本守卫写在**语句**里（不是"先查再写"），重复点击只有第一次能匹配上
	if !strings.Contains(sql, "WHERE id = ? AND status = ? AND urgent = ?") {
		t.Fatalf("条件更新应把 status/urgent 守卫写进 WHERE，实得：\n%s", sql)
	}
	// 绑定参数里同时有要写的 true 和当守卫的 false
	args := f.writtenArgs()
	for _, want := range []string{"true", "false", "7"} {
		if !strings.Contains(args, want) {
			t.Fatalf("绑定参数里缺少 %q：%s", want, args)
		}
	}

	// 2) 投出去的那份带着加急标记，且 URL/Stage/ID 来自库里那一行
	select {
	case task := <-got:
		if !task.Urgent {
			t.Error("投进快车道的任务应带 Urgent 标记")
		}
		if task.ID != 7 || task.Stage != "stub" || task.URL != "https://example.com" {
			t.Fatalf("投递的任务应来自库里那一行，实得 %+v", task)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("任务没进快车道")
	}

	// 3) 进了去重表，后续正常投递不会重复入队
	if !e.dedupCache.Get("stub|https://example.com") {
		t.Error("加急投递后应进内存去重表")
	}
}

func TestUrgentTaskRejects(t *testing.T) {
	cases := []struct {
		name string
		prep func(*fakeTaskDB)
		want error
	}{
		{
			"已在执行中（已被取走）",
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusProcessing) },
			ErrTaskUrgent,
		},
		{
			"已成功",
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusSuccess) },
			ErrTaskFinished,
		},
		{
			"已失败",
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusFailed) },
			ErrTaskFinished,
		},
		{
			"行已被删掉",
			func(f *fakeTaskDB) { f.noRows = true },
			ErrTaskNotFound,
		},
		{
			"阶段未注册",
			func(f *fakeTaskDB) { f.row["stage"] = "nope" },
			ErrStageNotRegistered,
		},
		{
			"重复加急：条件更新没匹配上（urgent 已经是 true）",
			func(f *fakeTaskDB) { f.row["urgent"] = true; f.affected = 0 },
			ErrTaskUrgent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			tc.prep(f)
			e, pool := urgentEngine(t, f)

			err := e.UrgentTask(7)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// 被拒的都不该往池子里投
			if main, urgent := pool.QueueDepths(); main+urgent != 0 {
				t.Fatalf("被拒的任务不该入队，实得 main/urgent = %d/%d", main, urgent)
			}
		})
	}
}

// 重复加急的守卫靠语句本身：WHERE 里有 urgent = false，第二次点击 RowsAffected 为 0 就被拒。
// 这条把「幂等检测」钉在 SQL 层，与 taskadmin_test.go 的 scope 断言互补。
func TestUrgentTaskSecondClickIsRejected(t *testing.T) {
	f := newFakeTaskDB()
	e, pool := urgentEngine(t, f)

	// 第一次成功
	if err := e.UrgentTask(7); err != nil {
		t.Fatalf("第一次加急 = %v, want nil", err)
	}
	// 第二次：库里 urgent 已是 1，条件更新匹配不到行
	f.mu.Lock()
	f.affected = 0
	f.mu.Unlock()

	if err := e.UrgentTask(7); !errors.Is(err, ErrTaskUrgent) {
		t.Fatalf("第二次加急 = %v, want ErrTaskUrgent", err)
	}
	// 两次点击只有第一次真的进了队列
	if main, urgent := pool.QueueDepths(); main+urgent != 1 {
		t.Fatalf("队列里应有且只有 1 条，实得 main/urgent = %d/%d", main, urgent)
	}
}
