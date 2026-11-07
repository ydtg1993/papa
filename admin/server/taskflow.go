package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/engine"
	"gorm.io/gorm"
	"net/http"
	"time"
)

//go:embed taskflow.js
var TaskFlowScript string

//go:embed taskflow.css
var TaskFlowCSS string

func (s *Monitor) taskFlowHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.cfg.TaskFlow == nil || s.cfg.UpdateTaskFlow == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 64 {
		http.Error(w, "运行 ID 无效", 400)
		return
	}
	if auth.OperatorFrom(ctx) == "" {
		http.Error(w, "需要具名访问令牌", 403)
		return
	}
	var err error
	var result any
	if r.Method == "POST" {
		var input struct {
			Revision uint64            `json:"revision"`
			Policy   engine.FlowPolicy `json:"policy"`
		}
		if agentBody(w, r, &input) != nil || input.Revision == 0 || input.Policy.Validate() != nil {
			http.Error(w, "策略或版本无效", 400)
			return
		}
		err = s.cfg.UpdateTaskFlow(ctx, id, input.Revision, input.Policy)
		if s.cfg.OnTaskFlowAction != nil {
			s.cfg.OnTaskFlowAction(AgentActionEvent{Action: "task-flow-policy", ID: id, Operator: auth.OperatorFrom(ctx), IP: clientIP(r).String(), At: time.Now(), Err: err})
		}
		result = map[string]any{"updated": err == nil, "revision": input.Revision + 1}
	} else {
		result, err = s.cfg.TaskFlow(ctx, id)
	}
	if err != nil {
		code, msg := 500, "调度操作失败，请刷新核对状态"
		switch {
		case errors.Is(err, engine.ErrFlowConflict):
			code, msg = 409, "策略已变更，请刷新后重新编辑"
		case errors.Is(err, engine.ErrFlowInvalid):
			code, msg = 400, "调度未启用或参数无效"
		case errors.Is(err, gorm.ErrRecordNotFound):
			code, msg = 404, "调度运行不存在"
		case errors.Is(err, context.DeadlineExceeded):
			code, msg = 504, "操作超时，请刷新核对"
		}
		http.Error(w, msg, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
