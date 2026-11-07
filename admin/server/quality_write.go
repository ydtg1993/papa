package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/pkg/agentadmin"
	"github.com/ydtg1993/papa/v3/pkg/rulequality"
)

type QualityReportWriter interface {
	Freeze(context.Context, rulequality.FreezeInput, string) (rulequality.Report, error)
	ReportOperation(context.Context, string) (rulequality.Report, error)
}

func (s *Monitor) qualityWrite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, "/api/quality/")
	method := "GET"
	keys := ""
	if path == "freeze" {
		method = "POST"
	}
	if path == "report-operation" {
		keys = "id"
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", 405)
		return
	}
	q, err := agentQuery(r, keys)
	if err != nil {
		s.agentError(w, agentadmin.ErrInvalid)
		return
	}
	writer, ok := s.cfg.QualityAdmin.(QualityReportWriter)
	if path == "capabilities" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"freeze": ok})
		return
	}
	if !ok {
		s.agentError(w, agentadmin.ErrDisabled)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result rulequality.Report
	if path == "report-operation" {
		result, err = writer.ReportOperation(ctx, q.Get("id"))
	} else {
		operator := auth.OperatorFrom(ctx)
		if !ruleText(operator, 128, false) {
			http.Error(w, "固化报告需要具名访问令牌，请先登录", 403)
			return
		}
		var in struct {
			OperationID string `json:"operation_id"`
			InputSHA256 string `json:"input_sha256"`
			Query       struct {
				Scope struct {
					Site       string `json:"site"`
					Stage      string `json:"stage"`
					RuleID     string `json:"rule_id"`
					Version    string `json:"version"`
					ReleaseID  string `json:"release_id"`
					Generation string `json:"release_generation"`
				} `json:"scope"`
				From  time.Time `json:"from"`
				Until time.Time `json:"until"`
				Limit int       `json:"limit"`
				Lost  uint64    `json:"lost_observations"`
			} `json:"query"`
		}
		err = agentBody(w, r, &in)
		if err == nil {
			var generation uint64
			generation, err = qualityUint(url.Values{"release_generation": {in.Query.Scope.Generation}}, "release_generation")
			if err == nil {
				sc := in.Query.Scope
				result, err = writer.Freeze(ctx, rulequality.FreezeInput{OperationID: in.OperationID, InputSHA256: in.InputSHA256, Query: rulequality.Query{Scope: rulequality.Scope{Site: sc.Site, Stage: sc.Stage, RuleID: sc.RuleID, Version: sc.Version, ReleaseID: sc.ReleaseID, ReleaseGeneration: generation}, From: in.Query.From, Until: in.Query.Until, Limit: in.Query.Limit, LostObservations: in.Query.Lost}}, operator)
				if s.cfg.OnAgentAction != nil {
					s.cfg.OnAgentAction(AgentActionEvent{Action: "freeze-report", ID: result.ID, OperationID: in.OperationID, Operator: operator, IP: clientIP(r).String(), At: time.Now(), Err: err})
				}
			}
		}
	}
	if err != nil {
		if errors.Is(err, rulequality.ErrReportChanged) {
			w.Header().Set("X-Quality-Outcome", "review-changed")
		}
		s.agentError(w, err)
		return
	}
	writeQualityJSON(w, result)
}
