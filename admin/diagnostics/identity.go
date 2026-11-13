package diagnostics

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/admin/internal/httpjson"
	"github.com/ydtg1993/papa/v4/internal/browserdiagnostics"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"net/http"
	"time"
)

// Service is the diagnostic capability consumed by these HTTP routes.
type Service interface {
	Begin(context.Context) (context.Context, func(), bool)
	View(context.Context, string) (map[string]any, error)
	Apply(context.Context, IdentityInput) (ActionResult, error)
	Rod(context.Context) (RodReport, error)
}
type Handler struct {
	Service Service
	Sites   func() any
	Audit   func(*http.Request, string, string, string, error)
}
type IdentityInput = browserdiagnostics.Input
type ActionResult = browserdiagnostics.ActionResult
type RodReport = browserdiagnostics.RodReport
type IdentityDiagnosticReport = browserdiagnostics.IdentityDiagnosticReport

// View adds navigation context to the service-owned identity snapshot. It is
// also used by the account-pool adapter without duplicating this presentation.
func (a *Handler) View(ctx context.Context, site string) (map[string]any, error) {
	view, err := a.Service.View(ctx, site)
	if view != nil && a.Sites != nil {
		view["sites"] = a.Sites()
	}
	return view, err
}

func (a *Handler) Identities(w http.ResponseWriter, r *http.Request) {
	ctx, done, ok := a.Service.Begin(r.Context())
	if !ok {
		httpjson.Write(w, 503, map[string]any{"error": "浏览器身份管理正在停止"})
		return
	}
	defer done()
	r = r.WithContext(ctx)
	if r.Method == http.MethodGet {
		view, err := a.View(r.Context(), r.URL.Query().Get("site"))
		if err != nil {
			identityAPIError(w, err)
			return
		}
		httpjson.Write(w, 200, view)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input IdentityInput
	if err := httpjson.Strict(http.MaxBytesReader(w, r.Body, 64<<10), &input); err != nil {
		httpjson.Write(w, 400, map[string]any{"error": "无效身份管理请求"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	result, err := a.Service.Apply(ctx, input)
	if result.Attempted && a.Audit != nil {
		a.Audit(r, "browser_identity", input.Action, input.AuditKey(), err)
	}

	if err != nil {
		identityAPIError(w, err)
		return
	}
	httpjson.Write(w, 200, map[string]any{"result": result.Value})
}

func identityAPIError(w http.ResponseWriter, err error) {
	status := 503
	var invalid *browserdiagnostics.InvalidInput
	if errors.As(err, &invalid) {
		status = 400
	}
	if errors.Is(err, browserdiagnostics.ErrBusy) {
		status = 409
	}
	if errors.Is(err, identityruntime.ErrConflict) {
		status = 409
	}
	if errors.Is(err, identityruntime.ErrNotFound) {
		status = 404
	}
	if errors.Is(err, identityruntime.ErrVerification) {
		status = 422
	}
	if errors.Is(err, identity.ErrConflict) || errors.Is(err, identity.ErrIncompatible) {
		status = 422
	}
	httpjson.Write(w, status, map[string]any{"error": browserdiagnostics.Redact(err.Error(), nil)})
}

// RodStep is one reported browser startup/cleanup check.
type RodStep = browserdiagnostics.RodStep

// IdentityCheck is one identity consistency comparison.
type IdentityCheck = browserdiagnostics.IdentityCheck

// IdentityDiagnosticSummary is the bounded, credential-free report history item.
type IdentityDiagnosticSummary = browserdiagnostics.IdentityDiagnosticSummary
