package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

// QualityAdministration is an optional read-only view. It never starts an Agent.
type QualityAdministration interface {
	Observations(context.Context, rulequality.AdminFilter) (rulequality.ObservationPage, error)
	Reports(context.Context, rulequality.AdminFilter) (rulequality.ReportPage, error)
	Observation(context.Context, string, string) (rulequality.Observation, error)
	Report(context.Context, string) (rulequality.Report, error)
	Summary(context.Context, rulequality.Query) (rulequality.Report, error)
}

func qualityUint(q url.Values, key string) (uint64, error) {
	raw := q.Get(key)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != raw {
		return 0, rulequality.ErrInvalid
	}
	return n, nil
}
func (s *Monitor) qualityHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.QualityAdmin == nil {
		http.Error(w, "质量监控未启用，请检查 quality.enabled 或宿主接线", 404)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/quality/")
	allowed := map[string]string{
		"observations": "site stage rule_id version from until before cutoff", "reports": "site stage rule_id version from until before_report",
		"observation": "id report_id", "report": "id", "summary": "site stage rule_id version release_id release_generation from until",
	}
	keys, ok := allowed[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	for key, values := range q {
		if len(values) != 1 || !strings.Contains(" "+keys+" ", " "+key+" ") {
			err = rulequality.ErrInvalid
		}
	}
	if err != nil {
		s.qualityError(w, rulequality.ErrInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result any
	if path == "observation" || path == "report" {
		if !ruleText(q.Get("id"), 128, false) || !ruleText(q.Get("report_id"), 128, true) {
			err = rulequality.ErrInvalid
		} else if path == "observation" {
			result, err = s.cfg.QualityAdmin.Observation(ctx, q.Get("id"), q.Get("report_id"))
		} else {
			result, err = s.cfg.QualityAdmin.Report(ctx, q.Get("id"))
		}
	} else {
		var f rulequality.AdminFilter
		f.Site, f.Stage, f.RuleID, f.Version = q.Get("site"), q.Get("stage"), q.Get("rule_id"), q.Get("version")
		f.From, err = time.Parse(time.RFC3339Nano, q.Get("from"))
		if err == nil {
			f.Until, err = time.Parse(time.RFC3339Nano, q.Get("until"))
		}
		if err == nil {
			f.Before, err = qualityUint(q, "before")
		}
		if err == nil {
			f.Cutoff, err = qualityUint(q, "cutoff")
		}
		f.BeforeReport = q.Get("before_report")
		if err == nil {
			err = f.Validate()
		}
		if err != nil {
			err = rulequality.ErrInvalid
		} else {
			switch path {
			case "observations":
				result, err = s.cfg.QualityAdmin.Observations(ctx, f)
			case "reports":
				result, err = s.cfg.QualityAdmin.Reports(ctx, f)
			case "summary":
				var generation uint64
				generation, err = qualityUint(q, "release_generation")
				if err == nil && (f.Site == "" || f.Stage == "" || f.RuleID == "" || f.Version == "" || !ruleText(q.Get("release_id"), 128, true) || q.Get("release_id") == "" && generation != 0) {
					err = rulequality.ErrInvalid
				}
				if err == nil {
					result, err = s.cfg.QualityAdmin.Summary(ctx, rulequality.Query{Scope: rulequality.Scope{Site: f.Site, Stage: f.Stage, RuleID: f.RuleID, Version: f.Version, ReleaseID: q.Get("release_id"), ReleaseGeneration: generation}, From: f.From, Until: f.Until, Limit: rulequality.AdminSummaryLimit})
				}
			}
		}
	}
	if err != nil {
		s.qualityError(w, err)
		return
	}
	// Preserve all 64-bit identifiers across JSON.parse; never convert via float64.
	raw, err := json.Marshal(result)
	if err != nil {
		s.qualityError(w, err)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err = dec.Decode(&value); err != nil {
		s.qualityError(w, err)
		return
	}
	qualityIdentifiers(value)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
func qualityIdentifiers(value any) {
	switch v := value.(type) {
	case map[string]any:
		for k, item := range v {
			switch k {
			case "sequence", "cutoff", "next", "release_generation", "task_id":
				if n, ok := item.(json.Number); ok {
					v[k] = n.String()
				}
			}
			qualityIdentifiers(item)
		}
	case []any:
		for _, item := range v {
			qualityIdentifiers(item)
		}
	}
}
func (s *Monitor) qualityError(w http.ResponseWriter, err error) {
	code, message := 500, "质量数据查询失败，请检查服务端状态后重试"
	switch {
	case errors.Is(err, rulequality.ErrInvalid):
		code, message = 400, "参数无效：时间范围须为最多 7 天，汇总须提供完整规则和发布范围"
	case errors.Is(err, rulequality.ErrNotFound):
		code, message = 404, "记录不存在或已过保留期；报告中的历史引用仍保留"
	case errors.Is(err, rulequality.ErrCorrupt), errors.Is(err, rulequality.ErrEvidence), errors.Is(err, rulequality.ErrIncompatible):
		code, message = 409, "数据完整性或证据关联校验未通过，不能展示为有效依据"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code, message = 504, "质量数据查询超时或已取消，请缩小范围重试"
	}
	http.Error(w, message, code)
}
