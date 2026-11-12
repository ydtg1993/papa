package diagnostics_test

import (
	"context"
	"github.com/ydtg1993/papa/v4/admin/diagnostics"
	"testing"
)

// An external adapter can implement the capability without importing internals.
type customService struct{}

func (customService) Begin(ctx context.Context) (context.Context, func(), bool) {
	return ctx, func() {}, true
}
func (customService) View(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (customService) Apply(context.Context, diagnostics.IdentityInput) (diagnostics.ActionResult, error) {
	return diagnostics.ActionResult{}, nil
}
func (customService) Rod(context.Context) (diagnostics.RodReport, error) {
	return diagnostics.RodReport{Steps: []diagnostics.RodStep{{Name: "custom", Status: "ok"}}}, nil
}

var _ diagnostics.Service = customService{}

func TestPublicDiagnosticContracts(t *testing.T) {
	report := diagnostics.IdentityDiagnosticReport{IdentityDiagnosticSummary: diagnostics.IdentityDiagnosticSummary{OK: true}, Checks: []diagnostics.IdentityCheck{{Field: "custom", Status: "passed"}}}
	if !report.OK || len(report.Checks) != 1 {
		t.Fatal("public result construction failed")
	}
}
