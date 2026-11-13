package breakers

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/incident"
	"io"
	"net/http"
	"sort"
	"strings"
)

type Logger interface{ Infof(string, ...any) }
type Options struct {
	BreakerStatuses      func() map[string]core.BreakerStatus
	BreakerReports       func(context.Context, string) ([]incident.Lifecycle, error)
	ResumeBreaker        func(string, string) bool
	ForceResumeBreaker   func(string) bool
	PauseBreaker         func(string, string) bool
	OnBreakerResume      func(string, string, string)
	OnBreakerForceResume func(string, string)
	OnBreakerPause       func(string, string, string)
}
type Handler struct {
	cfg    Options
	logger Logger
}

func New(cfg Options, logger Logger) *Handler { return &Handler{cfg: cfg, logger: logger} }
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func SortedStatuses(all map[string]core.BreakerStatus) []core.BreakerStatus {
	sites := make([]string, 0, len(all))
	for site := range all {
		sites = append(sites, site)
	}
	sort.Strings(sites)
	out := make([]core.BreakerStatus, 0, len(sites))
	for _, site := range sites {
		st := all[site]
		st.Site = site // 键就是站点，别让调用方再从 map 键去猜
		out = append(out, st)
	}
	return out
}

func (s *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"breakers": SortedStatuses(s.cfg.BreakerStatuses())})
}

func (s *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ResumeBreaker == nil || s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}

	// 要放行谁：`?scope=xxx&stage=yyy` 或请求体同名字段；都不给 = **全部站点**（整站那一层）。
	// 多站时"全部"是横幅上那个「恢复抓取」，单站时与以前完全一样。
	site, stage, ok := TargetParam(r)
	if !ok {
		http.Error(w, "bad request: scope/stage must be strings (stage 需要 scope)", http.StatusBadRequest)
		return
	}
	known := s.cfg.BreakerStatuses()
	if site != "" {
		if _, exists := known[site]; !exists {
			http.Error(w, "unknown scope: "+site, http.StatusBadRequest)
			return
		}
	}

	targets := []string{site}
	if site == "" {
		targets = targets[:0]
		for key := range known {
			targets = append(targets, key)
		}
		sort.Strings(targets)
	}
	resumed := make([]string, 0, len(targets))
	for _, key := range targets {
		if s.cfg.ResumeBreaker(key, stage) {
			resumed = append(resumed, key)
		}
	}
	if len(resumed) == 0 {
		writeJSON(w, map[string]any{"status": "ok", "resumed": false, "note": "未变更：当前无需恢复，或探针尚未退出"})
		return
	}
	operator := auth.OperatorFrom(r.Context())
	for _, key := range resumed {
		if s.cfg.OnBreakerResume != nil {
			s.cfg.OnBreakerResume(operator, key, stage)
		}
	}
	s.logger.Infof("已请求重试恢复/解除人工暂停（操作人 %q，scope=%q stage=%q）", operator, strings.Join(resumed, ","), stage)
	// resumed 仍是**布尔**（前端/脚本靠它区分"本来就没暂停"）；放行了哪些 scope 在 resumed_scopes 里。
	out := map[string]any{"status": "ok", "resumed": true, "resumed_scopes": resumed}
	if stage != "" {
		out["stage"] = stage
	}
	writeJSON(w, out)
}

func (s *Handler) ForceResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ForceResumeBreaker == nil || s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	site, stage, ok := TargetParam(r)
	if !ok || site == "" || stage != "" {
		http.Error(w, "force release requires one site and no stage", http.StatusBadRequest)
		return
	}
	if _, exists := s.cfg.BreakerStatuses()[site]; !exists {
		http.Error(w, "unknown scope", http.StatusBadRequest)
		return
	}
	changed := s.cfg.ForceResumeBreaker(site)
	if changed {
		operator := auth.OperatorFrom(r.Context())
		if s.cfg.OnBreakerForceResume != nil {
			s.cfg.OnBreakerForceResume(operator, site)
		}
		s.logger.Infof("人工强制解除保护（操作人 %q，site=%q）；未经健康验证，保留失败计数", operator, site)
	}
	writeJSON(w, map[string]any{"status": "ok", "resumed": changed, "mode": "force", "note": "强制解除不代表健康验证通过，失败计数保留"})
}

func (s *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.PauseBreaker == nil || s.cfg.BreakerStatuses == nil {
		http.Error(w, "breaker not configured", http.StatusNotFound)
		return
	}
	site, stage, ok := TargetParam(r)
	if !ok {
		http.Error(w, "bad request: scope/stage must be strings (stage 需要 scope)", http.StatusBadRequest)
		return
	}
	if site == "" {
		// 与 resume 的默认相反：这里不点名就是错的（见函数头）。文案要说清"不是静默停全部"。
		http.Error(w, "bad request: scope is required（暂停必须点名站点；不点名不会停掉全部）", http.StatusBadRequest)
		return
	}
	if _, exists := s.cfg.BreakerStatuses()[site]; !exists {
		http.Error(w, "unknown scope: "+site, http.StatusBadRequest)
		return
	}
	if !s.cfg.PauseBreaker(site, stage) {
		// 本来就在暂停态：不报错（重复点击不该看起来像失败），也不记审计（没有状态变更）——
		// 与 resume 的 resumed=false 那条同一取向。
		note := "该站点本来就在暂停态"
		if stage != "" {
			note = "该站点或这个阶段本来就在暂停态"
		}
		writeJSON(w, map[string]any{"status": "ok", "paused": false, "note": note})
		return
	}
	operator := auth.OperatorFrom(r.Context())
	if s.cfg.OnBreakerPause != nil {
		s.cfg.OnBreakerPause(operator, site, stage)
	}
	s.logger.Infof("已手动暂停（操作人 %q，scope=%q stage=%q），worker 不再取任务", operator, site, stage)
	out := map[string]any{"status": "ok", "paused": true, "paused_scopes": []string{site}}
	if stage != "" {
		out["stage"] = stage
	}
	writeJSON(w, out)
}

func TargetParam(r *http.Request) (site, stage string, ok bool) {
	q := r.URL.Query()
	site, stage = q.Get("scope"), q.Get("stage")
	if r.Body != nil {
		var body struct {
			Scope *string `json:"scope"`
			Stage *string `json:"stage"`
		}
		// 空请求体是合法的（"全部放行"）：先看一眼有没有内容，避免把 io.EOF 当解析失败。
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<10))
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return "", "", false
		}
		if body.Scope != nil {
			site = *body.Scope
		}
		if body.Stage != nil {
			stage = *body.Stage
		}
	}
	if stage != "" && site == "" {
		return "", "", false
	}
	return site, stage, true
}
