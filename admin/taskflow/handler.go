package taskflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/access"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/engine"
	"gorm.io/gorm"
	"io"
	"net/http"
	"time"
)

// taskFlowBody 拒绝超限、重复键、未知字段和多份 JSON，避免策略确认产生歧义。
func taskFlowBody(w http.ResponseWriter, r *http.Request, out any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		return err
	}
	if !json.Valid(b) {
		return errors.New("invalid JSON")
	}
	if err := taskFlowUniqueKeys(json.NewDecoder(bytes.NewReader(b)), 0); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func taskFlowUniqueKeys(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || seen[key] {
				return errors.New("duplicate JSON key")
			}
			seen[key] = true
		}
		if err := taskFlowUniqueKeys(d, depth+1); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func (s *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		if taskFlowBody(w, r, &input) != nil || input.Revision == 0 || input.Policy.Validate() != nil {
			http.Error(w, "策略或版本无效", 400)
			return
		}
		err = s.cfg.UpdateTaskFlow(ctx, id, input.Revision, input.Policy)
		if s.cfg.OnTaskFlowAction != nil {
			s.cfg.OnTaskFlowAction(TaskFlowActionEvent{Action: "task-flow-policy", ID: id, Operator: auth.OperatorFrom(ctx), IP: access.ClientIP(r).String(), At: time.Now(), Err: err})
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

type TaskFlowActionEvent struct {
	Action   string
	ID       string
	Operator string
	IP       string
	At       time.Time
	Err      error
}
type Options struct {
	TaskFlow         func(context.Context, string) (engine.TaskFlowView, error)
	UpdateTaskFlow   func(context.Context, string, uint64, engine.FlowPolicy) error
	OnTaskFlowAction func(TaskFlowActionEvent)
}
type Handler struct{ cfg Options }

func New(cfg Options) *Handler { return &Handler{cfg: cfg} }
