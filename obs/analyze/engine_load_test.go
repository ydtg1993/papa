package analyze

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/internal/gormfake"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

func TestLoadEngineStateFromDB(t *testing.T) {
	fake := gormfake.New()
	fake.SetRow(map[string]driver.Value{
		"cfg_key": "sites",
		"cfg_val": []byte(`[{"key":"a","stage_count":2,"breaker_paused":true,"breaker_paused_at":"2026-10-28T22:27:39.7861894+08:00","pause_source":"manual","pause_reason":"站点声明 StartPaused"},{"key":"b","stage_count":3}]`),
	})
	db := gormfake.MustOpen(fake)
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, time.Local)
	evs := []events.Event{
		{Type: events.TypeStageSnapshot, TS: now.Add(-time.Minute)}, // 空 stages = 无活跃池
	}
	st := LoadEngineState(context.Background(), db, evs, now)
	if st.Source == "" || st.Source == "none" {
		t.Fatalf("有 DB 时应 Source=db，实得 %q", st.Source)
	}
	if len(st.Sites) != 2 || !st.Sites[0].Paused {
		t.Fatalf("站点暂停状态没解析出来：%+v", st.Sites)
	}
	if st.Sites[0].PauseSource != "manual" || st.Sites[0].PauseReason != "站点声明 StartPaused" {
		t.Fatalf("DB 快照的暂停来源/原因丢失: %+v", st.Sites[0])
	}
	if st.ActivePools != 0 {
		t.Fatalf("空 stages 快照应 ActivePools=0，实得 %d", st.ActivePools)
	}
}

func TestParseSiteStatesLegacyPauseDoesNotGuessSource(t *testing.T) {
	sites, err := parseSiteStates([]byte(`[{"key":"old","breaker_paused":true}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || !sites[0].Paused || sites[0].PauseSource != "" || sites[0].PauseReason != "" {
		t.Fatalf("旧数据应该保留来源未知: %+v", sites)
	}
}

func TestLoadEngineStateNoDBFallsBackToEvents(t *testing.T) {
	st := LoadEngineState(context.Background(), nil, nil, time.Now())
	if st.Source != "none" {
		t.Fatalf("无 DB 无事件时 Source = %q, want none", st.Source)
	}
}
