package monitoring

import (
	"crypto/rand"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/admin/breakers"
	"github.com/ydtg1993/papa/v4/admin/sysinfo"
	"github.com/ydtg1993/papa/v4/core"
	"net/http"
	"time"
)

type Options struct {
	Stages          func() map[string]core.StageStats
	QueueStats      func() map[string]core.QueueStat
	BreakerStatuses func() map[string]core.BreakerStatus
	SiteStats       func() map[string]core.SiteStat
	SysInfo         *sysinfo.Collector
}
type Handler struct {
	cfg        Options
	instanceID string
	getter     func() map[string]core.StageStats
}

func New(cfg Options) *Handler {
	return &Handler{cfg: cfg, instanceID: rand.Text(), getter: cfg.Stages}
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Handler) Snapshot(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"instance_id": s.instanceID,
		"stages":      s.StageData(),
	}
	if s.cfg.QueueStats != nil {
		resp["queues"] = s.cfg.QueueStats()
	}
	// 熔断状态跟着 /api/monitor 一起回：前端每轮刷新只发一个请求就能画出横幅，
	// 不必再单开一条轮询。单独的 GET /api/breaker 留给脚本/外部系统。
	if s.cfg.BreakerStatuses != nil {
		// 是**数组**不是单个对象：每个站点一条状态（暂停与否都在里面），后台要能逐站看、逐站放行。
		// 顺序固定（按站点名字典序），前端就不用再排。
		resp["breakers"] = breakers.SortedStatuses(s.cfg.BreakerStatuses())
	}
	if s.cfg.SiteStats != nil {
		resp["sites"] = s.cfg.SiteStats()
	}
	if s.cfg.SysInfo != nil {
		resp["system"] = s.cfg.SysInfo.Snapshot()
	}
	// 这是响应组装时间；系统指标和治理积压仍各自携带真实采样时间。
	resp["generated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	writeJSON(w, resp)
}

func (s *Handler) StageData() map[string]any {
	monitors := s.getter()
	out := make(map[string]any, len(monitors))
	for stage, ss := range monitors {
		out[stage] = map[string]any{
			// 裸阶段名（`GetStage()` 原值）与所属站点：站点的 Tab 靠 site 分组，
			// 标题显示 stage；键 `site:stage` 只用于唯一标识这一格。
			"stage":   ss.Stage,
			"site":    ss.Site,
			"global":  ss.Global,
			"workers": ss.Workers,
			"queue": map[string]any{
				"submitted":   ss.Queue.Submitted,
				"completed":   ss.Queue.Completed,
				"failed":      ss.Queue.Failed,
				"in_progress": ss.Queue.InProgress,
				"queue_len":   ss.Queue.QueueLen,
			},
		}
	}
	return out
}
