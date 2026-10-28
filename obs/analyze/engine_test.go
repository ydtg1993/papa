package analyze

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func engineInput() *obs.Input {
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	return &obs.Input{
		Mode: obs.ModeSnapshot,
		Now:  now,
		Engine: obs.EngineState{Source: "engine", CapturedAt: now, ActivePools: 0,
			Sites: []obs.SiteState{{Key: "a", Paused: true, StageCount: 2}}},
		Events: []events.Event{
			{Type: events.TypeTaskRequeued, TS: now.Add(-5 * time.Minute), Queue: "recover_queue", Site: "a", Stage: "detail"},
		},
	}
}

func TestEngineSectionPausedAndRecover(t *testing.T) {
	res, err := (&EngineSection{Querier: &fakeQuerier{old: 379, sites: []obs.SiteState{{Key: "a", Paused: true}}}}).Build(context.Background(), engineInput())
	if err != nil {
		t.Fatal(err)
	}
	if c := checkByID(res, "engine_paused"); c == nil || c.Passed {
		t.Fatalf("engine_paused 应 warn 未通过：%+v", c)
	}
	if c := checkByID(res, "recover_in_progress"); c == nil || !c.Passed {
		t.Fatalf("recover_in_progress 应通过（有 requeue + 旧 processing）：%+v", c)
	}
	for _, id := range []string{"stage_pools", "engine_idle"} {
		if c := checkByID(res, id); c == nil {
			t.Fatalf("缺少检查 %s", id)
		}
	}
	if !strings.Contains(res.Summary, "来源 engine") {
		t.Fatalf("Summary 应说明引擎快照来源：%q", res.Summary)
	}
	joined := tableText(res)
	for _, want := range []string{"recover_queue", "DB 旧 processing", "站点状态"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("引擎段应包含 %q：%s", want, joined)
		}
	}
}

// 注入的实时快照缺失时，站点表回落到 Querier 的 DB 快照。
func TestEngineSectionFallsBackToQuerierSites(t *testing.T) {
	in := engineInput()
	in.Engine.Sites = nil
	res, err := (&EngineSection{Querier: &fakeQuerier{sites: []obs.SiteState{{Key: "db-site", Paused: true}}}}).Build(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tableText(res), "db-site") {
		t.Fatalf("站点快照应回落到 Querier：%s", tableText(res))
	}
	if c := checkByID(res, "engine_paused"); c == nil || c.Passed {
		t.Fatalf("DB 快照里有暂停站点时 engine_paused 不应通过：%+v", c)
	}
}

// Querier 报错不拖垮本节：降级写进 in.Degraded，已拿到的数据照常出表。
func TestEngineSectionQuerierErrorDegrades(t *testing.T) {
	in := engineInput()
	in.Engine.Sites = nil
	res, err := (&EngineSection{Querier: &fakeQuerier{err: errors.New("boom")}}).Build(context.Background(), in)
	if err != nil {
		t.Fatalf("Querier 报错不应让本节失败：%v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
	var siteErr, oldErr bool
	for _, d := range in.Degraded {
		if strings.Contains(d, "站点快照查询失败") {
			siteErr = true
		}
		if strings.Contains(d, "旧 processing 查询失败") {
			oldErr = true
		}
	}
	if !siteErr || !oldErr {
		t.Fatalf("降级信息不全：%v", in.Degraded)
	}
}
