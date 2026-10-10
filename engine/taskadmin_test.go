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
		{
			// 这一列本身就是版本守卫：要开就必须现在关着（反之亦然），
			// 重复点击影响 0 行 → 409，不需要先查再写。
			"开轮询：守卫 = 现在关着，写入 1",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return setRepeatableScope(tx, 7, models.RepeatableNo).Update("repeatable", models.RepeatableYes)
				})
			},
			[]string{"UPDATE", "id = 7", "repeatable = 0", "`repeatable`=1"},
		},
		{
			"停轮询：守卫 = 现在开着，写入 0",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return setRepeatableScope(tx, 7, models.RepeatableYes).Update("repeatable", models.RepeatableNo)
				})
			},
			[]string{"UPDATE", "id = 7", "repeatable = 1", "`repeatable`=0"},
		},
		{
			// 改周期的版本号是"旧周期值"（行快照里带的那个），重复点击只有第一次能匹配上
			"设轮询周期：守卫 = 旧周期值",
			func() string {
				return db.ToSQL(func(tx *gorm.DB) *gorm.DB {
					return setRepeatIntervalScope(tx, 7, 600).Update("repeat_interval", 60)
				})
			},
			[]string{"UPDATE", "id = 7", "repeat_interval = 600", "`repeat_interval`=60"},
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
	e := &Engine{
		db:         db,
		loggerSet:  &loggers.LoggerSet{Engine: logrus.New(), DB: logrus.New()},
		cfg:        &config.Config{},
		stages:     map[string]*stageInfo{"stub": {workerPool: pool}},
		dedupCache: newDedupCache(0),
	}
	// 与 NewEngine 一致：运行期覆盖层总得有个零值，否则读它的路径（如 repeatQueueConfig）会 nil 解引用
	return e, pool
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

/* ---------- 后台「开 / 停轮询」 ---------- */

// 开关只有一条语句：条件更新 repeatable 列（守卫写在语句里，不是先查再写），
// 且**不**顺手重投 —— 想立刻跑一次用「重投」。
func TestSetTaskRepeatableHappyPath(t *testing.T) {
	cases := []struct {
		name       string
		on         bool
		prep       func(*fakeTaskDB)
		wantTarget int64 // SET 的目标值
		wantGuard  int64 // WHERE 里的守卫值（要被换掉的那个旧值）
	}{
		{"开轮询：关 → 开", true, nil, int64(models.RepeatableYes), int64(models.RepeatableNo)},
		{"停轮询：开 → 停", false, nil, int64(models.RepeatableNo), int64(models.RepeatableYes)},
		{
			// WHERE 里刻意没有 status 守卫：停轮询最常见的用法正是"这条还在跑，跑完这次别再轮询了"
			"正在跑的行也停得掉",
			false,
			func(f *fakeTaskDB) { f.row["status"] = int64(models.TaskStatusProcessing) },
			int64(models.RepeatableNo), int64(models.RepeatableYes),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			if tc.prep != nil {
				tc.prep(f)
			}
			e, pool := urgentEngine(t, f)

			if err := e.SetTaskRepeatable(7, tc.on); err != nil {
				t.Fatalf("SetTaskRepeatable(7, %v) = %v, want nil", tc.on, err)
			}
			sql := f.written()
			if !strings.Contains(sql, "UPDATE `crawler_tasks`") || !strings.Contains(sql, "`repeatable`") {
				t.Fatalf("应条件更新 repeatable 列，实得：\n%s", sql)
			}
			if !strings.Contains(sql, "WHERE id = ? AND repeatable = ?") {
				t.Fatalf("守卫应写在语句里（不是先查再写）：\n%s", sql)
			}
			// 「开轮询」= 排期置为现在（下一轮扫描就投它）；「停轮询」不碰排期
			if got := strings.Contains(sql, "`next_repeat_at`"); got != tc.on {
				t.Fatalf("next_repeat_at 写入 = %v, want %v（只在开轮询时置为现在）：\n%s", got, tc.on, sql)
			}
			// 方向：绑定参数里的整数就是 id / 目标值 / 守卫值（updated_at 是 time.Time，不在其中）
			got := map[int64]int{}
			for _, v := range f.args[0] {
				if n, ok := v.(int64); ok {
					got[n]++
				}
			}
			want := map[int64]int{7: 1, tc.wantTarget: 1, tc.wantGuard: 1}
			if len(got) != len(want) {
				t.Fatalf("绑定参数里的整数 = %v, want %v", got, want)
			}
			for n, c := range want {
				if got[n] != c {
					t.Fatalf("绑定参数里的整数 = %v, want %v", got, want)
				}
			}
			// 判断条件全写在那条 UPDATE 里（守卫断言在上面）；这里唯一允许的 SELECT 是
			// 「取行的 site，好叫醒它所属站点的轮询队列」—— 只读一列、不参与任何判断。
			if read := f.readSQL(); read != "" && !strings.Contains(read, "SELECT `site` FROM `crawler_tasks`") {
				t.Fatalf("除了取 site，开关不该先查再写，实得 SELECT：\n%s", read)
			}
			// 只改标记，不投任务（开了也不立刻重跑一次）
			if main, urgent := pool.QueueDepths(); main+urgent != 0 {
				t.Fatalf("开关不该入队，实得 main/urgent = %d/%d", main, urgent)
			}
		})
	}
}

// 被拒的四种情况：语句照发（带守卫）、0 行，然后冷路径回查把原因说清楚。
// 注意它**不该**进 TestRejectedAdminOpsWriteNothing —— 那条断言"被拒时一条 SQL 都没发"，
// 而这里的守卫正是写在语句里的（同 TestMarkTaskFailedTerminalGuardIsInStatement）。
func TestSetTaskRepeatableRejections(t *testing.T) {
	cases := []struct {
		name string
		on   bool
		prep func(*fakeTaskDB)
		want error
	}{
		{
			"已经开着又开",
			true,
			func(f *fakeTaskDB) {
				f.row["repeatable"] = int64(models.RepeatableYes)
				f.affected = 0
			},
			ErrTaskRepeatOn,
		},
		{
			"本来就没开又停",
			false,
			func(f *fakeTaskDB) {
				f.row["repeatable"] = int64(models.RepeatableNo)
				f.affected = 0
			},
			ErrTaskRepeatOff,
		},
		{
			"行已被删掉",
			true,
			func(f *fakeTaskDB) { f.noRows = true; f.affected = 0 },
			ErrTaskNotFound,
		},
		{
			// 0 行、回查时这一列却是"守卫值"：说明有人在 UPDATE 与回查之间又改回去了
			"并发下被改回相反值",
			true,
			func(f *fakeTaskDB) {
				f.row["repeatable"] = int64(models.RepeatableNo)
				f.affected = 0
			},
			ErrTaskChanged,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			tc.prep(f)
			e, pool := urgentEngine(t, f)

			err := e.SetTaskRepeatable(7, tc.on)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if sql := f.written(); !strings.Contains(sql, "WHERE id = ? AND repeatable = ?") {
				t.Fatalf("守卫应写在语句里（被拒也不该是先查再写）：\n%s", sql)
			}
			if main, urgent := pool.QueueDepths(); main+urgent != 0 {
				t.Fatalf("被拒的开关不该入队，实得 main/urgent = %d/%d", main, urgent)
			}
		})
	}
}

// 「开轮询」不只是翻一列：排期置为"现在"（下一轮扫描就投它），并叫醒轮询队列重算节拍 ——
// 节拍是 pull 出来的，不叫这一声，它要等当前那次 sleep 到期（可能是一整个全局 interval）。
func TestSetTaskRepeatableWakesQueue(t *testing.T) {
	f := newFakeTaskDB()
	e, _ := urgentEngine(t, f)
	e.ensureRepeatQueue("") // 手搓的 Engine 没备队列键，唤醒得有地方落
	wake := e.repeatQueue(repeatQueueKey("")).wake

	if err := e.SetTaskRepeatable(7, true); err != nil {
		t.Fatalf("SetTaskRepeatable(7, true) = %v", err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("开轮询后应叫醒轮询队列重算节拍")
	}
}

/* ---------- 后台「设轮询周期」 ---------- */

// 一条语句改两列：周期本身 + 按新周期重算的下次到点（否则改短了还要按旧排期等）。
func TestSetTaskRepeatIntervalHappyPath(t *testing.T) {
	cases := []struct {
		name    string
		was     int
		seconds int
		global  time.Duration
		eff     int64 // 写进 SQL 的有效周期秒数（0 = 跟全局 → 取全局值）
	}{
		{"设成 10 分钟", 0, 600, 2 * time.Hour, 600},
		{"设成 0 = 跟全局", 600, 0, 2 * time.Hour, 7200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			e, _ := urgentEngine(t, f)
			e.cfg.RepeatQueue = config.RepeatQueueConfig{Enabled: true, Interval: tc.global}
			e.ensureRepeatQueue("") // 队列键得先备好，唤醒才有地方落
			wake := e.repeatQueue(repeatQueueKey("")).wake

			if err := e.SetTaskRepeatInterval(7, tc.was, tc.seconds); err != nil {
				t.Fatalf("SetTaskRepeatInterval = %v", err)
			}
			sql := f.written()
			if !strings.Contains(sql, "WHERE id = ? AND repeat_interval = ?") {
				t.Fatalf("版本守卫应写在语句里（不是先查再写）：\n%s", sql)
			}
			for _, want := range []string{"`repeat_interval`", "`next_repeat_at`", "FROM_UNIXTIME", "last_repeat_at"} {
				if !strings.Contains(sql, want) {
					t.Fatalf("SQL 里缺少 %q：\n%s", want, sql)
				}
			}
			// 绑定参数：id / 旧值 / 新周期 / 有效周期（都按整数比，不看顺序）
			got := map[int64]int{}
			for _, v := range f.args[0] {
				if n, ok := v.(int64); ok {
					got[n]++
				}
			}
			want := map[int64]int{7: 1, int64(tc.was): 1, int64(tc.seconds): 1}
			want[tc.eff]++
			if len(got) != len(want) {
				t.Fatalf("绑定参数里的整数 = %v, want %v", got, want)
			}
			for n, c := range want {
				if got[n] != c {
					t.Fatalf("绑定参数里的整数 = %v, want %v", got, want)
				}
			}
			select {
			case <-wake:
			default:
				t.Fatal("改周期后应叫醒轮询队列重算节拍")
			}
		})
	}
}

// 非法周期在写库之前就被挡下（不合法就别碰库），0 行与行不存在各有哨兵。
func TestSetTaskRepeatIntervalRejections(t *testing.T) {
	cases := []struct {
		name    string
		was     int
		seconds int
		prep    func(*fakeTaskDB)
		want    error
	}{
		{"负数", 0, -1, nil, ErrRepeatIntervalBad},
		{"小于最短刻度（minTick 内的静默取整不如直接拒）", 0, 5, nil, ErrRepeatIntervalBad},
		{"行已被删掉", 0, 600, func(f *fakeTaskDB) { f.noRows = true; f.affected = 0 }, ErrTaskNotFound},
		{"周期刚被改过（快照过期）", 0, 600, func(f *fakeTaskDB) { f.affected = 0 }, ErrTaskChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			if tc.prep != nil {
				tc.prep(f)
			}
			e, _ := urgentEngine(t, f)

			err := e.SetTaskRepeatInterval(7, tc.was, tc.seconds)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// 校验不通过的那两条：一条 SQL 都不该发
			if errors.Is(err, ErrRepeatIntervalBad) {
				if sql := f.written(); sql != "" {
					t.Fatalf("非法周期不该写库：\n%s", sql)
				}
				return
			}
			// 行不存在：取 site 那一步就早退了（连 UPDATE 都不用发）
			if errors.Is(err, ErrTaskNotFound) {
				if sql := f.written(); sql != "" {
					t.Fatalf("行不存在时不该写库：\n%s", sql)
				}
				return
			}
			if sql := f.written(); !strings.Contains(sql, "WHERE id = ? AND repeat_interval = ?") {
				t.Fatalf("守卫应写在语句里（被拒也不该是先查再写）：\n%s", sql)
			}
		})
	}
}
