package analyze

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/obs"
	"github.com/ydtg1993/papa/v3/pkg/events"
	"gorm.io/gorm"
)

// LoadEngineState 离线构造报告运行上下文：优先 DB 的 crawler_config.sites 快照，
// 再用事件补「最近快照时间 / 是否有阶段池 / 是否有 recover 批」。
// 没有 DB 也没有事件时返回 Source=none —— 调用方据此不做暂停判定。
//
// 离线与常驻的差别写在 Degraded 里：DB 那份 sites 是「现在」的投影，不保证是报告窗口内的状态；
// 事件推出来的阶段池数也只能取窗口内**最后一个** stage_snapshot，窗口外的建池/收池看不到。
func LoadEngineState(ctx context.Context, db *gorm.DB, evs []events.Event, now time.Time) obs.EngineState {
	st := obs.EngineState{Source: "none", CapturedAt: now}
	if db != nil {
		var row models.Config
		if db.WithContext(ctx).Where("cfg_key = ?", "sites").Take(&row).Error == nil {
			if states, err := parseSiteStates(row.CfgVal); err == nil {
				st.Sites = states
				st.Source = "db"
				st.Degraded = "站点暂停状态取当前 DB 快照，可能不是报告窗口内的状态"
			}
		}
	}
	for _, ev := range evs {
		switch ev.Type {
		case events.TypeStageSnapshot:
			// 窗口内有多条快照时以**最后一条**为准：空 stages 也是有效信息（引擎空闲/未建池），
			// 不能被更早那条「有池」的快照盖掉。
			if st.LastSnapshotAt.IsZero() || ev.TS.After(st.LastSnapshotAt) {
				st.LastSnapshotAt = ev.TS
				st.ActivePools = len(ev.Stages)
			}
		case events.TypeStagePool:
			// 没有 stage_snapshot（或最后一条快照说 0 个池）时，靠建池事件补一个「有池」信号；
			// reaped = 收池事件，不算活跃。
			if st.ActivePools == 0 && !strings.EqualFold(ev.Source, "reaped") {
				st.ActivePools = 1
			}
		case events.TypeTaskRequeued:
			if strings.Contains(strings.ToLower(ev.Queue), "recover") {
				st.Recovering = true
			}
		}
	}
	if st.Source != "db" && st.LastSnapshotAt.IsZero() && len(evs) == 0 {
		return st
	}
	if st.Source == "none" {
		st.Source = "events"
	}
	return st
}

// parseSiteStates 从 crawler_config.sites 的 JSON 解析站点快照（与 engine 同一 schema）。
// 排序保证报告输出稳定；空数组返回空切片而非 nil，调用方不用区分「没有站点」和「没解析出」。
func parseSiteStates(raw []byte) ([]obs.SiteState, error) {
	var sites []core.SiteStat
	if err := json.Unmarshal(raw, &sites); err != nil {
		return nil, fmt.Errorf("解析 sites 快照: %w", err)
	}
	states := make([]obs.SiteState, 0, len(sites))
	for _, s := range sites {
		states = append(states, obs.SiteState{Key: s.Key, Paused: s.BreakerPaused, PausedAt: s.BreakerPausedAt, StageCount: s.StageCount})
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Key < states[j].Key })
	return states, nil
}
