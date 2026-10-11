package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// siteEngine 造一个多站引擎：站点 a（自动轮询）+ 站点 b（显式关掉自动）+ 默认 scope。
func siteEngine(t *testing.T, f *fakeTaskDB) *Engine {
	t.Helper()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	// core.Site 上的是**生效值**（声明里的 *bool 已经在 snapshot() 里压成 bool 了）：
	// 两个站都显式给 true —— 这就是"声明里不写 AutoRepeat"解析出来的样子。
	// 要测"某站关掉自动"的用例自己再 SetSite 覆盖成 false。
	e.SetSite(core.Site{Key: "a", AutoRepeat: true})
	e.SetSite(core.Site{Key: "b", AutoRepeat: true})
	// 两个治理队列都按站点拆：站点登记完再把它们的键备齐
	e.ensureErrorQueues()
	e.ensureRepeatQueues()
	return e
}

// 队列名与站点的对应：默认 scope 保持历史名字，命名站点是 repeat_queue:<站点>；
// 只有登记过的站点才算数（免得打错的站名在后台多出一行"看起来开着"的假队列）。
func TestRepeatQueueKeyAndSiteRoundTrip(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)

	if got := repeatQueueKey(""); got != QueueRepeat {
		t.Fatalf("默认 scope 的键 = %q, want %q", got, QueueRepeat)
	}
	if got := repeatQueueKey("a"); got != "repeat_queue:a" {
		t.Fatalf("站点 a 的键 = %q", got)
	}
	// 顺序稳定：默认 scope 在最前，其余按站点名字典序
	if got := e.repeatQueueSites(); !reflect.DeepEqual(got, []string{"", "a", "b"}) {
		t.Fatalf("repeatQueueSites = %v, want [\"\" a b]", got)
	}
	for name, want := range map[string]struct {
		site string
		ok   bool
	}{
		QueueRepeat:            {"", true},
		"repeat_queue:a":       {"a", true},
		"repeat_queue:ghost":   {"", false}, // 没登记过的站名
		"repeat_queue:":        {"", false}, // 空站点名
		"error_queue":          {"", false},
		"repeat_queue:a:extra": {"", false},
	} {
		site, ok := e.repeatQueueSite(name)
		if ok != want.ok || site != want.site {
			t.Errorf("repeatQueueSite(%q) = %q/%v, want %q/%v", name, site, ok, want.site, want.ok)
		}
	}
}

// 到点那条查询带 site 与到点条件；「轮询任务」（全量）只差"不看周期"这一条。
// 假库不解析 WHERE，所以只能钉 SQL 形态（真过滤在 MySQL 那边）。
func TestRepeatQueryShapePerSite(t *testing.T) {
	cases := []struct {
		name  string
		run   func(*Engine)
		want  []string
		never []string
	}{
		{
			"到点：本站 + 可轮询 + 已完成 + 到点",
			func(e *Engine) { e.repeatQueueQuery("a")().Find(&[]models.CrawlerTask{}) },
			[]string{"repeatable = ?", "status IN", "site = ?", "next_repeat_at"},
			nil,
		},
		{
			"全量：本站 + 可轮询 + 已完成（**不看周期**）",
			func(e *Engine) { e.repeatForceQueueQuery("a")().Find(&[]models.CrawlerTask{}) },
			[]string{"repeatable = ?", "status IN", "site = ?"},
			[]string{"next_repeat_at"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTaskDB()
			e := siteEngine(t, f)
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

// 每站一个队列：运行快照与累计计数各记各的；没登记的站点报 ErrUnknownSite。
func TestRepollSiteQueuesKeepStatsApart(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	setSiteQueues(e, "a", SiteQueues{Repeat: config.RepeatQueueConfig{Enabled: true, BatchSize: 10, WorkerCount: 1}})

	if n, err := e.RepollSiteRepeatableTasks("a"); err != nil || n != 1 {
		t.Fatalf("RepollSiteRepeatableTasks(a) = %d, %v, want 1, nil", n, err)
	}
	stats := e.GetQueueStats()
	if got := stats["repeat_queue:a"]; got.Runs != 1 || got.TotalProcessed != 1 {
		t.Fatalf("站点 a 的快照 = %+v, want Runs=1 TotalProcessed=1", got)
	}
	if got := stats["repeat_queue:b"]; got.Runs != 0 || got.TotalProcessed != 0 {
		t.Fatalf("站点 b 不该被站点 a 的执影响到：%+v", got)
	}
	if _, ok := stats[QueueRepeat]; !ok {
		t.Fatal("默认 scope 那一行永远都在（老行的 site 是空串）")
	}

	if _, err := e.RepollSiteRepeatableTasks("ghost"); !errors.Is(err, ErrUnknownSite) {
		t.Fatalf("未知站点应报 ErrUnknownSite，实得 %v", err)
	}
}

// 站点级开关只关得掉"自动"：那一行显示已停用、ticker 那一跳不投，但两个手动入口照旧。
func TestAutoRepeatGatesAutomaticButNotManual(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	// 站点 b 在声明里关掉自动轮询（真实声明里就是 `AutoRepeat: &false`）—— 闸门读的是
	// **生效值**（e.siteStats），所以这里要照生产的路径播种一次，而不是只 SetSite。
	e.SetSite(core.Site{Key: "b"})
	e.seedSiteRows()
	setSiteQueues(e, "a", SiteQueues{Repeat: config.RepeatQueueConfig{Enabled: true, BatchSize: 10, WorkerCount: 1}})
	setSiteQueues(e, "", SiteQueues{Repeat: config.RepeatQueueConfig{Enabled: true, BatchSize: 10, WorkerCount: 1}})

	if e.queueEnabled("repeat_queue:b") {
		t.Fatal("关掉自动轮询的站，后台那一行应当是「已停用」")
	}
	if !e.queueEnabled("repeat_queue:a") || !e.queueEnabled(QueueRepeat) {
		t.Fatal("没关的站（含默认 scope）应显示启用")
	}
	if e.queueEnabled("repeat_queue:ghost") {
		t.Fatal("不认识的站名一律当停用")
	}
	if e.siteAutoRepeat("b") {
		t.Fatal("站点 b 的生效值应来自播种后的快照（声明里关着）")
	}
	if !e.siteAutoRepeat("a") {
		t.Fatal("没写 AutoRepeat 的站点应当按自动处理")
	}

	// 手动入口不看闸门：到点的与全量的都能点
	if n, err := e.RepollSiteRepeatableTasks("b"); err != nil || n != 1 {
		t.Fatalf("关掉自动的站仍应能手动手动触发（到点）：%d, %v", n, err)
	}
	if n, err := e.ForceRepollSiteRepeatableTasks("b"); err != nil || n != 1 {
		t.Fatalf("关掉自动的站仍应能「轮询任务」全量触发：%d, %v", n, err)
	}
}

// ticker 那一跳的闸门认的是**生效值**（库 + 运行期开关），不是声明快照：
// 库里关着就不自动跑（重启按库走），后台点一下暂停/恢复也要真的停、真的起。
func TestAutoRepeatGateStopsAutoTicks(t *testing.T) {
	f := newFakeTaskDB()
	f.siteRows = []fakeRow{siteRow("b", false)} // 库里 b 站是"暂停自动轮询"，而声明里它是自动
	e := siteEngine(t, f)
	e.seedSiteRows()
	pool := e.stages["stub"].workerPool
	pool.Start(e.ctx, func(context.Context, *Task) error { return nil })
	setSiteQueues(e, "b", SiteQueues{Repeat: config.RepeatQueueConfig{Enabled: true, Interval: 20 * time.Millisecond}})
	e.startRepeatQueue("b")

	if e.siteAutoRepeat("b") {
		t.Fatal("库为事实：行里 auto_repeat=0，声明里的「自动」不该把它翻回来（重启按库走）")
	}
	time.Sleep(80 * time.Millisecond)
	if got := e.GetQueueStats()["repeat_queue:b"].Runs; got != 0 {
		t.Fatalf("库关着的站不该被自动跑：Runs=%d", got)
	}

	// 后台「恢复自动轮询」：闸门必须真的开 —— 这一步以前对自动那一跳无效（闸门读的是声明快照）
	if err := e.SetSiteAutoRepeat("b", true); err != nil {
		t.Fatalf("恢复自动轮询 = %v", err)
	}
	if !e.siteAutoRepeat("b") {
		t.Fatal("恢复后闸门应打开（读的是生效值，不是声明快照）")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.GetQueueStats()["repeat_queue:b"].Runs == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.GetQueueStats()["repeat_queue:b"].Runs; got == 0 {
		t.Fatal("恢复自动后 ticker 仍未执行")
	}

	// 再暂停：已经排上队的那一轮跑完，之后不再有新的自动那一跳
	if err := e.SetSiteAutoRepeat("b", false); err != nil {
		t.Fatalf("暂停自动轮询 = %v", err)
	}
	before := e.GetQueueStats()["repeat_queue:b"].Runs
	time.Sleep(100 * time.Millisecond)
	if after := e.GetQueueStats()["repeat_queue:b"].Runs; after != before {
		t.Fatalf("暂停后不该再自动跑：Runs %d -> %d", before, after)
	}
}

// Stop 要等在途的那一跳：不等的话调用方看到 drained=true 就去关库，
// ticker 里正在写的语句会直接报 `sql: database is closed`。
func TestStopWaitsForTickers(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.stages["stub"].workerPool.Start(e.ctx, func(context.Context, *Task) error { return nil })
	setSiteQueues(e, "", SiteQueues{Repeat: config.RepeatQueueConfig{Enabled: true, Interval: 20 * time.Millisecond}})
	e.ensureRepeatQueue("")
	e.startRepeatQueue("")

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
// 回退成站点那句"最粗兜底"的周期（huangguo 是 2h）→ 自动轮询永远按站点周期跑，任务自己定的
// 短周期、以及"刚开轮询 / 刚改周期"的唤醒全都不生效（后台还显示 enabled=true，看不出坏）。
// 当初是靠临时日志才定位到的：`repeatTickInterval site="huangguo" enabled=true interval=2h0m0s d=0s ok=false`。
//
// 假库不解析 SQL、DryRun 只拼不执行，所以这里只能钉住"那层 CAST 还在"（值层面由真库跑一次证）——
// 但恰恰是"有人觉得 CAST 多余顺手删掉"这一种改法能被它拦住。
func TestSoonestRepeatQueryCastsToSignedInt(t *testing.T) {
	db := dryRunMySQL(t)
	// 用 ToSQL 把语句拼出来（DryRun 下 Scan 会报 "dry run mode unsupported"，Find 这条能出 SQL）
	var rows []struct{ Epoch *int64 }
	sql := db.ToSQL(func(tx *gorm.DB) *gorm.DB {
		return soonestRepeatQueryOn(tx, "huangguo").Find(&rows)
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
