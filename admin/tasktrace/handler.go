// Package tasktrace serves task timelines and trace snapshots.
package tasktrace

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/core"
	"net/http"
	"strconv"
	"strings"
)

type Logger interface{ Errorf(string, ...any) }

// Options supplies the trace and snapshot lookups, without execution state.
type Options struct {
	Snapshot func(context.Context, string) ([]byte, error)
	Trace    func(int) ([]core.TraceExecutionStep, error)
	Logger   Logger
}
type Handler struct {
	snapshot func(context.Context, string) ([]byte, error)
	trace    func(int) ([]core.TraceExecutionStep, error)
	logger   Logger
}

func New(o Options) *Handler {
	return &Handler{snapshot: o.Snapshot, trace: o.Trace, logger: o.Logger}
}

// Trace 返回最近保留的执行追踪时间线（不代表任务当前状态）；轮次与截断判据由追踪服务拥有。
func (s *Handler) Trace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trace == nil {
		http.Error(w, "task trace not configured", http.StatusNotFound)
		return
	}
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	if err != nil || id <= 0 {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	steps, err := s.trace(id)
	if err != nil {
		// 业务原因（如追踪开关没开）原样回给前端，抽屉里直接显示这句话
		s.logger.Errorf("load task trace %d: %s", id, err.Error())
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{"task_id": id, "steps": steps})
}

// Page downloads a trace snapshot identified by an opaque token.
func (s *Handler) Page(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.snapshot == nil {
		http.Error(w, "页面快照未配置", http.StatusNotFound)
		return
	}
	token := r.URL.Query().Get("file")
	if strings.TrimSpace(token) == "" {
		http.Error(w, "缺少 file 参数", http.StatusBadRequest)
		return
	}
	b, err := s.snapshot(r.Context(), token)
	if err != nil {
		http.Error(w, "页面快照不可用（可能已过保留期）", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="snapshot.bin"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
