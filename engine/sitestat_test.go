package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/internal/breaker"
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// siteRow 造一行站点表数据（"上次跑过一轮"的样子）。
func siteRow(key string, autoRepeat bool) fakeRow {
	return fakeRow{
		"id": int64(1), "key": key, "base_url": "https://declared.example/",
		"auto_repeat": autoRepeat, "stage_count": int64(1),
		"last_repeat_at": nil, "repeat_total": int64(7), "repeat_backlog": int64(2),
		"last_repeat_error": "上次的错误", "breaker_paused": false, "breaker_paused_at": nil,
		"created_at": time.Now(), "updated_at": time.Now(),
	}
}

// 启动播种：缺行按声明插（含 AutoRepeat）；已有行只刷新从声明来的两列，**不动 AutoRepeat** ——
// 否则运营在后台改过的开关会被每次重启悄悄翻回去。
func TestSeedSiteRowsKeepsOperatorChoice(t *testing.T) {
	f := newFakeTaskDB()
	f.siteRows = []fakeRow{siteRow("a", false)} // 库里 a 站是"关着自动轮询"（运营改的）
	e := siteEngine(t, f)                       // 声明里 a 站是自动
	e.seedSiteRows()

	stats := e.GetSiteStats()
	if len(stats) != 3 { // "" + a + b
		t.Fatalf("站点快照 = %+v", stats)
	}
	if stats["a"].AutoRepeat {
		t.Fatal("行已存在时不该用声明覆盖 AutoRepeat（库为事实）")
	}
	if !stats["b"].AutoRepeat {
		t.Fatal("库里没有 b 的行：应按声明插入（自动）")
	}
	if stats["a"].RepeatTotal != 7 || stats["a"].LastRepeatError != "上次的错误" {
		t.Fatalf("已有的统计列应读回来：%+v", stats["a"])
	}

	sql := f.written()
	if !strings.Contains(sql, "INSERT INTO `crawler_sites`") {
		t.Fatalf("缺行时应插入：\n%s", sql)
	}
	for _, stmt := range strings.Split(sql, "\n") {
		if strings.HasPrefix(stmt, "UPDATE") && strings.Contains(stmt, "auto_repeat") {
			t.Fatalf("刷新已有行时不该写 auto_repeat：\n%s", stmt)
		}
	}
}

// 快照是纯内存：监控页 3 秒一刷，调用它不该产生任何查询。
func TestGetSiteStatsDoesNotQuery(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.seedSiteRows()

	before := f.readSQL()
	if len(e.GetSiteStats()) == 0 {
		t.Fatal("播种后应当有站点快照")
	}
	if after := f.readSQL(); after != before {
		t.Fatalf("GetSiteStats 不该查库：\n%s", after)
	}
}

// 每站轮询跑完 → 只按列更新自己那行（不整行写回，免得盖掉运营改过的列），内存同步跟着走。
func TestRepollWritesSiteStats(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.seedSiteRows()
	setSiteQueues(e, "", SiteQueues{Repeat: config.RepeatQueueConfig{Enabled: true, BatchSize: 10, WorkerCount: 1}})

	if n, err := e.RepollSiteRepeatableTasks("a"); err != nil || n != 1 {
		t.Fatalf("RepollSiteRepeatableTasks(a) = %d, %v", n, err)
	}

	stats := e.GetSiteStats()
	if stats["a"].RepeatTotal != 1 || stats["a"].LastRepeatAt.IsZero() {
		t.Fatalf("站点 a 的统计应跟上本轮：%+v", stats["a"])
	}
	if stats["a"].LastRepeatError != "" {
		t.Fatalf("成功的一轮应清掉上次错误：%+v", stats["a"])
	}
	if stats["b"].RepeatTotal != 0 || !stats["b"].LastRepeatAt.IsZero() {
		t.Fatalf("站点 b 不该被站点 a 的执影响到：%+v", stats["b"])
	}

	sql := f.written()
	for _, want := range []string{"`repeat_total`", "`last_repeat_at`", "`repeat_backlog`"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("统计回写缺少 %q：\n%s", want, sql)
		}
	}
	for _, stmt := range strings.Split(sql, "\n") {
		if strings.HasPrefix(stmt, "UPDATE `crawler_sites`") && strings.Contains(stmt, "base_url") {
			t.Fatalf("回写统计时不该碰 base_url（那是声明抄来的列）：\n%s", stmt)
		}
	}
}

// 熔断暂停/恢复 → 站点表那两列跟着走（库与内存都改）。
func TestPauseSiteWritesBreakerState(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 3})
	e.seedSiteRows()

	if !e.PauseSite("a", "测试") {
		t.Fatal("暂停应当成功")
	}
	if got := e.GetSiteStats()["a"]; !got.BreakerPaused || got.BreakerPausedAt.IsZero() {
		t.Fatalf("内存快照应记下闸住状态：%+v", got)
	}
	if sql := f.written(); !strings.Contains(sql, "`breaker_paused`") {
		t.Fatalf("站住表应记下闸住状态：\n%s", sql)
	}

	if !e.ResumeSite("a") {
		t.Fatal("放行应当成功")
	}
	if got := e.GetSiteStats()["a"]; got.BreakerPaused || !got.BreakerPausedAt.IsZero() {
		t.Fatalf("放行后应清掉闸住状态：%+v", got)
	}
}

// 阶段统计要带上站点维度（后台的站点 Tab 就是按它分组的）。
func TestStageStatsCarrySite(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e.stages["stub"] = &stageInfo{workerPool: pool, config: StageConfig{Site: "a"}}
	e.setStatsQueue("stub", track.NewStatsQueue[*Task](pool))

	got := e.GetStageStats()
	if len(got) != 1 || got["stub"].Site != "a" {
		t.Fatalf("阶段快照应带上站点：%+v", got)
	}
}

// 站点级自动轮询开关（后台「站点」页那两个动作走它）：只改那一列 + 内存快照 + 叫醒该站队列；
// 重复点 409；默认 scope 不允许（它只能靠全局开关）。
func TestSetSiteAutoRepeat(t *testing.T) {
	f := newFakeTaskDB()
	f.siteRows = []fakeRow{siteRow("a", true)}
	e := siteEngine(t, f)
	e.seedSiteRows()

	if err := e.SetSiteAutoRepeat("a", false); err != nil {
		t.Fatalf("暂停自动轮询 = %v", err)
	}
	if got := e.GetSiteStats()["a"]; got.AutoRepeat {
		t.Fatalf("内存快照应跟上：%+v", got)
	}
	if sql := f.written(); !strings.Contains(sql, "`auto_repeat`") {
		t.Fatalf("站点表应记下这一列：\n%s", sql)
	}
	select {
	case <-e.repeatQueue(repeatQueueKey("a")).wake:
	default:
		t.Fatal("改完应叫醒该站的轮询队列（闸门在 onTick 里判，不叫要等下一次 sleep）")
	}

	// 重复点：库里已经是关着 → 条件更新 0 行 → 409
	f.mu.Lock()
	f.affected = 0
	f.mu.Unlock()
	if err := e.SetSiteAutoRepeat("a", false); !errors.Is(err, ErrSiteAlreadyManual) {
		t.Fatalf("重复暂停应报 ErrSiteAlreadyManual，实得 %v", err)
	}

	// 默认 scope 没有站点声明可挂
	if err := e.SetSiteAutoRepeat("", true); !errors.Is(err, ErrDefaultScopeNoAuto) {
		t.Fatalf("默认 scope 应报 ErrDefaultScopeNoAuto，实得 %v", err)
	}
}
