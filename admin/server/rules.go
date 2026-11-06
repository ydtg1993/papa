package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/pkg/rulestore"
)

// RuleAdministration is optional. Writes require an authenticated operator;
// the implementation owns durable generation checks and operation idempotency.
type RuleAdministration interface {
	Versions(context.Context, string, rulestore.Reference) (rulestore.VersionList, error)
	Inspect(context.Context, rulestore.Reference) (rulestore.VersionDetail, error)
	Releases(context.Context, string, uint64) (rulestore.ReleasePage, error)
	Verification(context.Context, string) (rulestore.Verification, error)
	Preview(context.Context, rulestore.RulePlan) (rulestore.RulePreview, error)
	Change(context.Context, rulestore.RuleChangeRequest, rulestore.Actor) (rulestore.ReleaseView, error)
	Operation(context.Context, string) (rulestore.ReleaseView, error)
}
type RuleChangeEvent struct {
	Request   rulestore.RuleChangeRequest
	Operator  string
	IP        string
	ReleaseID string
	Err       error
	At        time.Time
}

func ruleText(value string, max int, optional bool) bool {
	return (optional || value != "") && len(value) <= max && strings.TrimSpace(value) == value && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
func ruleBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil || dec.Decode(new(any)) != io.EOF {
		return rulestore.ErrAdminInput
	}
	return nil
}
func (s *Monitor) ruleAdminHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	method := http.MethodGet
	if path == "preview" || path == "change" {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.RuleAdmin == nil {
		http.Error(w, "持久规则运维未启用，请检查 rules.enabled 或宿主接线", 404)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var result any
	var err error
	q := r.URL.Query()
	ref := rulestore.Reference{ID: q.Get("id"), Version: q.Get("version")}
	switch path {
	case "versions":
		after := rulestore.Reference{ID: q.Get("after_id"), Version: q.Get("after_version")}
		if !ruleText(ref.ID, 128, true) || ((after.ID != "" || after.Version != "") && (!ruleText(after.ID, 128, false) || !ruleText(after.Version, 128, false))) {
			err = rulestore.ErrAdminInput
			break
		}
		result, err = s.cfg.RuleAdmin.Versions(ctx, ref.ID, after)
	case "version":
		if !ruleText(ref.ID, 128, false) || !ruleText(ref.Version, 128, false) {
			err = rulestore.ErrAdminInput
			break
		}
		result, err = s.cfg.RuleAdmin.Inspect(ctx, ref)
	case "verification", "operation":
		limit := 64
		if path == "operation" {
			limit = 128
		}
		if !ruleText(ref.ID, limit, false) {
			err = rulestore.ErrAdminInput
			break
		}
		if path == "verification" {
			result, err = s.cfg.RuleAdmin.Verification(ctx, ref.ID)
		} else {
			result, err = s.cfg.RuleAdmin.Operation(ctx, ref.ID)
		}
	case "releases":
		before := uint64(0)
		raw := q.Get("before")
		if raw != "" {
			before, err = strconv.ParseUint(raw, 10, 64)
			if err != nil || strconv.FormatUint(before, 10) != raw {
				err = rulestore.ErrAdminInput
				break
			}
		}
		if !ruleText(q.Get("site"), 128, false) {
			err = rulestore.ErrAdminInput
			break
		}
		result, err = s.cfg.RuleAdmin.Releases(ctx, q.Get("site"), before)
	case "preview":
		var plan rulestore.RulePlan
		if err = ruleBody(w, r, &plan); err == nil {
			_, err = plan.Validate()
		}
		if err == nil {
			result, err = s.cfg.RuleAdmin.Preview(ctx, plan)
		}
	case "change":
		operator := auth.OperatorFrom(ctx)
		if !ruleText(operator, 128, false) {
			http.Error(w, "发布和回滚需要具名访问令牌，请先登录", 403)
			return
		}
		var request rulestore.RuleChangeRequest
		if err = ruleBody(w, r, &request); err == nil {
			err = request.Validate()
		}
		if err == nil {
			var release rulestore.ReleaseView
			release, err = s.cfg.RuleAdmin.Change(ctx, request, rulestore.Actor{Operator: operator, Source: "oa"})
			result = release
			if s.cfg.OnRuleChange != nil {
				s.cfg.OnRuleChange(RuleChangeEvent{Request: request, Operator: operator, IP: clientIP(r).String(), ReleaseID: release.ID, Err: err, At: time.Now()})
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.ruleAdminError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
func (s *Monitor) ruleAdminError(w http.ResponseWriter, err error) {
	// Only errors known to precede/roll back the transaction authorize dropping
	// a saved operation. Integrity/read errors may happen after COMMIT.
	for _, rejected := range []error{rulestore.ErrAdminInput, rulestore.ErrGenerationConflict, rulestore.ErrReviewConflict, rulestore.ErrUnverified, rulestore.ErrNotPublished, rulestore.ErrNoRollback} {
		if errors.Is(err, rejected) {
			w.Header().Set("X-Rule-Outcome", "rejected")
			break
		}
	}
	code, message := 500, "规则操作结果未确认，请按操作 ID 核对；详细错误见服务端日志"
	switch {
	case errors.Is(err, rulestore.ErrAdminInput):
		code, message = 400, "规则查询或操作参数无效"
	case errors.Is(err, rulestore.ErrNotFound):
		code, message = 404, "未找到记录；核对操作时请保留原操作 ID"
	case errors.Is(err, rulestore.ErrGenerationConflict):
		code, message = 409, "发布代次已改变，请刷新并重新审阅"
	case errors.Is(err, rulestore.ErrReviewConflict):
		code, message = 409, "验收依据已改变，请重新预览和审阅"
	case errors.Is(err, rulestore.ErrOperationConflict):
		code, message = 409, "操作 ID 对应另一组参数或操作人，请核对原操作记录"
	case errors.Is(err, rulestore.ErrUnverified), errors.Is(err, rulestore.ErrNotPublished):
		code, message = 409, "目标规则缺少可用的通过验收依据"
	case errors.Is(err, rulestore.ErrNoRollback):
		code, message = 409, "当前发布没有可回滚的前驱"
	case errors.Is(err, rulestore.ErrCorrupt):
		code, message = 409, "规则或验收记录完整性异常，请检查持久记录"
	case errors.Is(err, context.DeadlineExceeded):
		code, message = 504, "规则请求超时；写入结果请按原操作 ID 核对"
	}
	if (code == 500 || code == 504) && s.logger != nil {
		s.logger.Errorf("rule administration: %v", err)
	}
	http.Error(w, message, code)
}
