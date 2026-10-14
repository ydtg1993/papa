package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// fakeSiteStore 站点快照落点的假实现：引擎只认 SiteStore 接口，所以这里一份内存就够了。
//
// **失败注入不放在这儿**：引擎侧对"落点写失败"的语义就是"记一条 WARN 继续"，
// 而真正的失败策略在 admin/configstore（先落库、失败也更新内存），那边用真假库测。
type fakeSiteStore struct {
	mu     sync.Mutex
	rows   []core.SiteStat
	loads  int // Sites() 被调了几次（用来钉"监控页每轮都读一次快照"这条接线）
	writes int // SetSites() 被调了几次（启动对齐：一致时不回写）
}

func (s *fakeSiteStore) Sites() ([]core.SiteStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return append([]core.SiteStat(nil), s.rows...), nil
}

func (s *fakeSiteStore) SetSites(rows []core.SiteStat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	s.rows = append([]core.SiteStat(nil), rows...)
	return nil
}

func (s *fakeSiteStore) UpdateSites(fn func(rows []core.SiteStat) []core.SiteStat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = fn(append([]core.SiteStat(nil), s.rows...))
	return nil
}

func (s *fakeSiteStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *fakeSiteStore) snapshot() []core.SiteStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.SiteStat(nil), s.rows...)
}

// siteEngine 造一个多站引擎：站点 a + 站点 b，并注入一份假落点。
func siteEngine(t *testing.T, f *fakeTaskDB) *Engine {
	t.Helper()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.SetSite(core.Site{Key: "a"})
	e.SetSite(core.Site{Key: "b"})
	e.SetSiteStore(&fakeSiteStore{})
	return e
}

// siteStoreOf 取引擎里那份假落点。
func siteStoreOf(e *Engine) *fakeSiteStore { return e.siteStore.(*fakeSiteStore) }

// 启动对齐：**库里没有那一行 → 按声明生成**；有且与声明一致 → 只载入内存（不回写）；
// 有但不一致（多站/少站/字段变了/残留暂停态）→ 按声明对齐着写一次。
func TestBuildSiteStatsAlignsWithDeclaration(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSite(core.Site{Key: "a", BaseURL: "https://a.example/"})
	e.SetSite(core.Site{Key: "b", BaseURL: "https://b.example/"})
	e.AddStage("catalog", StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 4}, &recordingFetcher{stage: "catalog"}, nil)
	store := siteStoreOf(e)

	// ① 一行都没有：按声明生成
	e.buildSiteStats()
	if store.writeCount() != 1 {
		t.Fatalf("没有这一行时该生成一次，实得 %d 次写", store.writeCount())
	}
	if got := store.snapshot(); len(got) != 2 || got[0].StageCount != 1 {
		t.Fatalf("生成的内容不对：%+v", got)
	}

	// ② 与声明一致：只载入，不回写
	e.buildSiteStats()
	if store.writeCount() != 1 {
		t.Fatalf("与声明一致时不该回写（启动不该让 updated_at 往前跳）：写了 %d 次", store.writeCount())
	}

	// ③ 库里多了个声明里没有的站 + 残留一个暂停态 → 对齐着写一次
	_ = store.SetSites([]core.SiteStat{
		{Key: "a", BaseURL: "https://old.example/", StageCount: 9, BreakerPaused: true},
		{Key: "ghost", BaseURL: "https://ghost.example/"},
	})
	e.buildSiteStats()
	got := store.snapshot()
	if len(got) != 2 {
		t.Fatalf("对齐后应当正好是声明里的两站（ghost 丢掉）：%+v", got)
	}
	if got[0].BaseURL != "https://a.example/" || got[0].StageCount != 1 || got[0].BreakerPaused {
		t.Fatalf("字段该按声明刷新、暂停态该归零：%+v", got[0])
	}
	if !strings.Contains(siteStatsDiff([]core.SiteStat{{Key: "ghost"}}, got), "声明里已没有") {
		t.Fatalf("差异说明该点出多出来的站：%s", siteStatsDiff([]core.SiteStat{{Key: "ghost"}}, got))
	}
}

// 启动按声明重建快照：站点键 / BaseURL / 阶段数都从声明来，暂停态一律"没暂停"。
func TestBuildSiteStatsSeedsFromDeclaration(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSite(core.Site{Key: "a", BaseURL: "https://a.example/"})
	e.AddStage("catalog", StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 4}, &recordingFetcher{stage: "catalog"}, nil)
	e.AddStage("detail", StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 4}, &recordingFetcher{stage: "detail"}, nil)

	e.buildSiteStats()

	rows := siteStoreOf(e).snapshot()
	if len(rows) != 2 {
		t.Fatalf("两个站都该在快照里：%+v", rows)
	}
	// 按站点键排序（页面与快照都省一次排序）
	if rows[0].Key != "a" || rows[1].Key != "b" {
		t.Fatalf("快照应按站点键排序：%+v", rows)
	}
	if rows[0].BaseURL != "https://a.example/" || rows[0].StageCount != 2 {
		t.Fatalf("声明里的 BaseURL / 阶段数该抄进快照：%+v", rows[0])
	}
	if rows[0].BreakerPaused || !rows[0].BreakerPausedAt.IsZero() {
		t.Fatalf("刚建的快照一律按「没暂停」：%+v", rows[0])
	}

	// 监控页那条路：GetSiteStats 走落点，返回按站点键可查的 map
	got := e.GetSiteStats()
	if len(got) != 2 || got["a"].StageCount != 2 || got["b"].Key != "b" {
		t.Fatalf("GetSiteStats 应当反映快照：%+v", got)
	}
	if siteStoreOf(e).loads == 0 {
		t.Fatal("GetSiteStats 应当从落点取（内存优先由落点那层保证）")
	}
}

// 没注入落点时退化：不 panic、不落库，快照为空（手工拼的 Engine / 部分单测走这条路）。
func TestSiteStatsWithoutStoreDegrade(t *testing.T) {
	f := newFakeTaskDB()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.SetSite(core.Site{Key: "a"})

	e.buildSiteStats() // 不 panic 就算过
	if got := e.GetSiteStats(); len(got) != 0 {
		t.Fatalf("没落点时快照为空：%+v", got)
	}
	if e.PauseSite("a", "停了") { // 站点没配抓取策略 → 本来就返回 false
		t.Fatal("没 SetSiteBreaker 的站点不该暂停成功")
	}
}

// 暂停/恢复 → 快照里那一站的状态跟着走（暂停时间 = 那一刻；恢复时清掉）。
func TestPauseSiteWritesBreakerState(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 3})
	e.buildSiteStats()

	if !e.PauseSite("a", "测试") {
		t.Fatal("暂停应当成功")
	}
	if got := e.GetSiteStats()["a"]; !got.BreakerPaused || got.BreakerPausedAt.IsZero() {
		t.Fatalf("快照应记下闸住状态：%+v", got)
	}
	// 没被动过的那一站不受影响（整块替换最容易在这里出错）
	if got := e.GetSiteStats()["b"]; got.BreakerPaused {
		t.Fatalf("别的站不该被带上：%+v", got)
	}

	if !e.ResumeSite("a") {
		t.Fatal("放行应当成功")
	}
	if got := e.GetSiteStats()["a"]; got.BreakerPaused || !got.BreakerPausedAt.IsZero() {
		t.Fatalf("放行后应清掉闸住状态：%+v", got)
	}
}

// 并发暂停两个站：整块 JSON 的读-改-写不能互相盖（落点那层用一把写锁包住 RMW 就是为了这个）。
func TestPauseTwoSitesConcurrentlyKeepsBoth(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	e.SetSiteBreaker("b", breaker.Config{Enabled: false, Threshold: 1})
	e.buildSiteStats()

	var wg sync.WaitGroup
	for _, site := range []string{"a", "b"} {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			e.PauseSite(s, "并发停")
		}(site)
	}
	wg.Wait()

	got := e.GetSiteStats()
	if !got["a"].BreakerPaused || !got["b"].BreakerPaused {
		t.Fatalf("两个站都该被记成暂停（别互相盖）：%+v", got)
	}
}

// 阶段统计要带上站点维度（后台的站点 Tab 就是按它分组的）。
func TestStageStatsCarrySite(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e.stages["a:stub"] = &stageInfo{stage: "stub", config: StageConfig{Site: "a"}}
	e.setStatsQueue("a:stub", track.NewStatsQueue[*Task](pool))

	got := e.GetStageStats()
	if len(got) != 1 || got["a:stub"].Site != "a" || got["a:stub"].Stage != "stub" {
		t.Fatalf("阶段快照应带上站点与裸阶段名：%+v", got)
	}
}

// 上面几条已经换成"落点里看到什么"，这条钉住不再有多余的写：快照那两列不走 SQL 了。
func TestSiteSnapshotNoSQL(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.SetSiteBreaker("a", breaker.Config{Enabled: false, Threshold: 1})
	e.buildSiteStats()
	e.PauseSite("a", "停")
	e.ResumeSite("a")

	if sql := f.written(); strings.Contains(strings.ToLower(sql), "crawler_sites") {
		t.Fatalf("站点快照不再走 crawler_sites 表：\n%s", sql)
	}
	if sql := f.readSQL(); strings.Contains(strings.ToLower(sql), "crawler_sites") {
		t.Fatalf("站点快照不再查 crawler_sites 表：\n%s", sql)
	}
}
