// Package executionpolicy owns the execution policy HTTP adapter and settings UI.
package executionpolicy

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/internal/httpjson"
	policy "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
)

//go:embed static
var Assets embed.FS

type Service interface {
	Read(context.Context) (policy.View, error)
	Validate(context.Context, policy.Document) (policy.Preview, error)
	Update(context.Context, policy.Update, string) (policy.View, error)
	History(context.Context, int) ([]policy.History, error)
	Operation(context.Context, string) (policy.Operation, error)
	Rollback(context.Context, policy.Rollback, string) (policy.View, error)
}
type Options struct {
	Service Service
	// Authorize checks read/write access after the shared authentication layer.
	// nil retains existing full-administrator credentials' rights.
	Authorize func(*http.Request, string) error
}
type Handler struct{ Options }

func New(o Options) *Handler { return &Handler{o} }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if auth.OperatorFrom(r.Context()) == "" {
		httpjson.Write(w, 401, map[string]any{"error": "authentication required"})
		return
	}
	action := "read"
	if r.Method != http.MethodGet {
		action = "write"
	}
	if h.Authorize != nil {
		if err := h.Authorize(r, action); err != nil {
			httpjson.Write(w, 403, map[string]any{"error": "execution policy access denied"})
			return
		}
	}
	if action == "write" && r.Header.Get("Origin") != "" {
		u, e := url.Parse(r.Header.Get("Origin"))
		if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			httpjson.Write(w, 403, map[string]any{"error": "cross-origin write rejected"})
			return
		}
	}
	if h.Service == nil {
		httpjson.Write(w, 503, map[string]any{"error": "execution policy service unavailable"})
		return
	}
	var value any
	var err error
	path := r.URL.Path
	decode := func(v any) bool {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if e == nil {
			body = bytes.TrimSpace(body)
			if len(body) == 0 || body[0] != '{' {
				e = errors.New("request body must be a JSON object")
			} else {
				e = httpjson.Strict(bytes.NewReader(body), v)
			}
		}
		if e != nil {
			httpjson.Write(w, 400, map[string]any{"error": e.Error(), "kind": "invalid"})
			return false
		}
		return true
	}
	switch {
	case path == "/api/config/execution-policy" && r.Method == http.MethodGet:
		value, err = h.Service.Read(r.Context())
	case path == "/api/config/execution-policy" && r.Method == http.MethodPut:
		var in policy.Update
		if !decode(&in) {
			return
		}
		value, err = h.Service.Update(r.Context(), in, auth.OperatorFrom(r.Context()))
	case path == "/api/config/execution-policy/validate" && r.Method == http.MethodPost:
		var in policy.Document
		if !decode(&in) {
			return
		}
		value, err = h.Service.Validate(r.Context(), in)
	case path == "/api/config/execution-policy/history" && r.Method == http.MethodGet:
		limit := 50
		if s := r.URL.Query().Get("limit"); s != "" {
			limit, err = strconv.Atoi(s)
			if err != nil || limit < 1 || limit > 100 {
				httpjson.Write(w, 400, map[string]any{"error": "limit must be 1..100"})
				return
			}
		}
		value, err = h.Service.History(r.Context(), limit)
	case path == "/api/config/execution-policy/rollback" && r.Method == http.MethodPost:
		var in policy.Rollback
		if !decode(&in) {
			return
		}
		value, err = h.Service.Rollback(r.Context(), in, auth.OperatorFrom(r.Context()))
	case strings.HasPrefix(path, "/api/config/operations/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "/api/config/operations/")
		if id == "" || strings.Contains(id, "/") {
			httpjson.Write(w, 400, map[string]any{"error": "invalid operation_id"})
			return
		}
		value, err = h.Service.Operation(r.Context(), id)
	case path == "/api/config/capabilities" && r.Method == http.MethodGet:
		value = map[string]any{"domains": []string{"execution_policy"}, "schema_version": 1, "writable_fields": []string{"breaker.defaults", "breaker.sites", "browser.pool_size", "browser.direct_pool_size", "browser.max_idle_time"}, "constraints": map[string]any{"pool_size": "> 0", "direct_pool_size": ">= 0", "max_idle_time": ">= 0, duration string", "breaker": "positive explicit values; registered sites only; recovery budget relationships validated"}, "semantics": []string{"CAS full document", "active events retain frozen policy", "shrinking drains borrowed browsers", "disabled browser remains disabled"}}
	default:
		httpjson.Write(w, 405, map[string]any{"error": "method not allowed"})
		return
	}
	if err != nil {
		h.failure(w, r, err)
		return
	}
	httpjson.Write(w, 200, value)
}

func (h *Handler) failure(w http.ResponseWriter, r *http.Request, err error) {
	status := 503
	body := map[string]any{"error": err.Error(), "code": "unavailable"}
	var typed *policy.Error
	if errors.As(err, &typed) {
		body["code"] = typed.Code
		switch typed.Code {
		case "invalid":
			status = 400
		case "conflict":
			status = 409
		case "not_found":
			status = 404
		case "stopping":
			status = 503
		}
	}
	if status == 409 {
		if latest, e := h.Service.Read(r.Context()); e == nil {
			body["latest"] = latest
		}
	}
	httpjson.Write(w, status, body)
}
