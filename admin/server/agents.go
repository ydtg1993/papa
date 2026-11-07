package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/rulequality"
	"github.com/ydtg1993/papa/v4/pkg/rulestore"
)

// AgentAdministration intentionally has no publication method that mutates state.
type AgentAdministration interface {
	Capabilities() agentadmin.Capabilities
	Runs(context.Context, agentstore.RunFilter) (agentadmin.RunPage, error)
	Run(context.Context, string, uint64) (agentadmin.RunDetail, error)
	Requests(context.Context, string, string) (agentadmin.RequestPage, error)
	Request(context.Context, string) (agentadmin.RequestView, error)
	Operation(context.Context, string) (agentadmin.RequestView, error)
	Preview(context.Context, agentadmin.Selection, string) (agentadmin.Preview, error)
	Prepare(context.Context, agentadmin.PrepareInput, string) (agentadmin.RequestView, error)
	Start(context.Context, agentadmin.StartInput, string) (agentadmin.RequestView, error)
	Dismiss(context.Context, agentadmin.ControlInput, string) (agentadmin.RequestView, error)
	Cancel(context.Context, agentadmin.ControlInput, string) (agentadmin.RequestView, error)
	Reconcile(context.Context, agentadmin.ControlInput, string) (agentadmin.RequestView, error)
	Review(context.Context, string, rulestore.Reference) (agentadmin.Review, error)
	Publication(context.Context, string, rulestore.Reference) (rulestore.RulePlan, error)
}
type AgentActionEvent struct {
	Action, ID, OperationID, Operator, IP string
	Err                                   error
	At                                    time.Time
}

// Reject duplicate object keys at every depth before decoding a bounded DTO.
func agentBody(w http.ResponseWriter, r *http.Request, v any) error {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return agentadmin.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return agentadmin.ErrInvalid
				}
				seen[k] = true
				if e = value(); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = value(); e != nil {
					return e
				}
			}
		default:
			return agentadmin.ErrInvalid
		}
		_, e = d.Token()
		return e
	}
	if value() != nil {
		return agentadmin.ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return agentadmin.ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return agentadmin.ErrInvalid
	}
	return nil
}
func agentQuery(r *http.Request, keys string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	for k, v := range q {
		if len(v) != 1 || !strings.Contains(" "+keys+" ", " "+k+" ") {
			err = agentadmin.ErrInvalid
		}
	}
	return q, err
}
func (s *Monitor) agentHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	allowed := map[string]string{"capabilities": "", "runs": "site status before from until", "run": "id after", "requests": "before report_id", "request": "id", "operation": "id", "review": "run_id id version", "publication": "run_id id version", "preview": "", "prepare": "", "start": "", "cancel": "", "reconcile": "", "dismiss": ""}
	keys, ok := allowed[path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	write := path == "preview" || path == "prepare" || path == "start" || path == "cancel" || path == "reconcile" || path == "dismiss"
	method := "GET"
	if write {
		method = "POST"
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.AgentAdmin == nil {
		http.Error(w, "Agent 中心未启用，请检查 agent.enabled 或宿主接线", 404)
		return
	}
	q, err := agentQuery(r, keys)
	if err != nil {
		s.agentError(w, agentadmin.ErrInvalid)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	operator := auth.OperatorFrom(ctx)
	if write && !ruleText(operator, 128, false) {
		http.Error(w, "此操作需要具名访问令牌，请先登录", 403)
		return
	}
	var result any
	ev := AgentActionEvent{Action: path, Operator: operator, IP: clientIP(r).String(), At: time.Now()}
	attempted := false
	switch path {
	case "capabilities":
		result = s.cfg.AgentAdmin.Capabilities()
	case "runs":
		f := agentstore.RunFilter{Site: q.Get("site"), Status: q.Get("status"), Before: q.Get("before")}
		f.From, err = time.Parse(time.RFC3339Nano, q.Get("from"))
		if err == nil {
			f.Until, err = time.Parse(time.RFC3339Nano, q.Get("until"))
		}
		if err != nil {
			err = agentadmin.ErrInvalid
		} else if err = f.Validate(); err == nil {
			result, err = s.cfg.AgentAdmin.Runs(ctx, f)
		}
	case "run":
		var after uint64
		after, err = qualityUint(q, "after")
		if err == nil {
			result, err = s.cfg.AgentAdmin.Run(ctx, q.Get("id"), after)
		}
	case "requests":
		result, err = s.cfg.AgentAdmin.Requests(ctx, q.Get("before"), q.Get("report_id"))
	case "request":
		result, err = s.cfg.AgentAdmin.Request(ctx, q.Get("id"))
	case "operation":
		result, err = s.cfg.AgentAdmin.Operation(ctx, q.Get("id"))
	case "review", "publication":
		ref := rulestore.Reference{ID: q.Get("id"), Version: q.Get("version")}
		if path == "review" {
			result, err = s.cfg.AgentAdmin.Review(ctx, q.Get("run_id"), ref)
		} else {
			result, err = s.cfg.AgentAdmin.Publication(ctx, q.Get("run_id"), ref)
		}
	case "preview":
		var in agentadmin.Selection
		if err = agentBody(w, r, &in); err == nil {
			result, err = s.cfg.AgentAdmin.Preview(ctx, in, operator)
		}
	case "prepare":
		var in agentadmin.PrepareInput
		if err = agentBody(w, r, &in); err == nil {
			attempted = true
			ev.OperationID = in.OperationID
			result, err = s.cfg.AgentAdmin.Prepare(ctx, in, operator)
		}
	case "start":
		var in agentadmin.StartInput
		if err = agentBody(w, r, &in); err == nil {
			attempted = true
			ev.ID = in.ID
			result, err = s.cfg.AgentAdmin.Start(ctx, in, operator)
		}
	case "cancel", "reconcile", "dismiss":
		var in agentadmin.ControlInput
		if err = agentBody(w, r, &in); err == nil {
			attempted = true
			ev.ID = in.ID
			if path == "dismiss" {
				result, err = s.cfg.AgentAdmin.Dismiss(ctx, in, operator)
			} else if path == "cancel" {
				result, err = s.cfg.AgentAdmin.Cancel(ctx, in, operator)
			} else {
				result, err = s.cfg.AgentAdmin.Reconcile(ctx, in, operator)
			}
		}
	}
	if attempted && s.cfg.OnAgentAction != nil {
		ev.Err = err
		s.cfg.OnAgentAction(ev)
	}
	if err != nil {
		s.agentError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if path == "start" {
		w.WriteHeader(http.StatusAccepted)
	}
	_ = json.NewEncoder(w).Encode(result)
}
func (s *Monitor) agentError(w http.ResponseWriter, err error) {
	status, message := 500, "结果未确认，请按原操作 ID 或请求 ID 核对；不要创建替代执行"
	switch {
	case errors.Is(err, agentadmin.ErrInvalid), errors.Is(err, agentstore.ErrInvalid), errors.Is(err, rulequality.ErrInvalid):
		status, message = 400, "参数无效；请重新选择范围和分析配置"
	case errors.Is(err, agentadmin.ErrOwner):
		status, message = 403, "操作人与原请求或批准配置不一致"
	case errors.Is(err, agentstore.ErrNotFound), errors.Is(err, rulequality.ErrNotFound), errors.Is(err, rulestore.ErrNotFound):
		status, message = 404, "未找到记录；请保留原操作 ID 继续核对"
	case errors.Is(err, agentadmin.ErrDisabled):
		status, message = 503, "宿主尚未注册此能力或分析配置"
	case errors.Is(err, agentadmin.ErrBusy):
		status, message = 409, "本机执行器忙或正在停止，请先核对原请求"
	case errors.Is(err, agentadmin.ErrStopped):
		status, message = 409, "尚未证明原执行器已停止；不能接管或补记终态"
	case errors.Is(err, agentadmin.ErrReviewChanged), errors.Is(err, rulequality.ErrReportChanged):
		status, message = 409, "审阅依据已变化，请重新读取并预览"
	case errors.Is(err, rulequality.ErrNotActionable), errors.Is(err, rulequality.ErrEvidence), errors.Is(err, rulequality.ErrIncompatible), errors.Is(err, rulestore.ErrUnverified):
		status, message = 409, "报告、独立验收或证据不满足操作条件"
	case errors.Is(err, rulequality.ErrConflict), errors.Is(err, agentstore.ErrConflict):
		status, message = 409, "请求状态、版本或原操作参数不一致，请核对原请求"
	case errors.Is(err, rulequality.ErrCorrupt), errors.Is(err, agentstore.ErrCorrupt), errors.Is(err, rulestore.ErrCorrupt):
		status, message = 409, "持久记录完整性校验未通过"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, message = 504, "管理请求超时；后台执行状态请按原 ID 核对"
	}
	http.Error(w, message, status)
}
