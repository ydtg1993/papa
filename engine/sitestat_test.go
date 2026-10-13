package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
	"github.com/ydtg1993/papa/v3/internal/track"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
)

// siteRow 造一行站点表数据（key/base_url/stage_count/breaker 状态）。
func siteRow(key string) fakeRow {
	return fakeRow{
		"id": int64(1), "key": key, "base_url": "https://declared.example/",
		"stage_count": int64(1), "breaker_paused": false, "breaker_paused_at": nil,
		"created_at": time.Now(), "updated_at": time.Now(),
	}
}

// siteEngine 造一个多站引擎：站点 a + 站点 b + 默认 scope（""）。
func siteEngine(t *testing.T, f *fakeTaskDB) *Engine {
	t.Helper()
	e := queueEngine(t, f, workerpool.NewWorkerPool[*Task](1, 8, 1))
	e.SetSite(core.Site{Key: "a"})
	e.SetSite(core.Site{Key: "b"})
	return e
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
		t.Fatalf("站点表应记下闸住状态：\n%s", sql)
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
	e.stages["a:stub"] = &stageInfo{stage: "stub", config: StageConfig{Site: "a"}}
	e.setStatsQueue("a:stub", track.NewStatsQueue[*Task](pool))

	got := e.GetStageStats()
	if len(got) != 1 || got["a:stub"].Site != "a" || got["a:stub"].Stage != "stub" {
		t.Fatalf("阶段快照应带上站点与裸阶段名：%+v", got)
	}
}
