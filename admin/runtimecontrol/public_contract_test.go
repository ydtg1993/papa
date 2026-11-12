package runtimecontrol_test

import (
	"context"
	control "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"net/http/httptest"
	"testing"
)

// This consumer names only public packages/types, as a separate module must.
type externalPolicy struct{}

var _ control.PolicyService = externalPolicy{}

func (externalPolicy) Read(context.Context) control.Result {
	return control.Result{Kind: control.Unavailable, Value: map[string]any{"error": "not initialized"}}
}
func (externalPolicy) Save(context.Context, control.Update, string) control.Result {
	return control.Result{Kind: control.Invalid}
}
func (externalPolicy) Acknowledge(string, uint64) control.Result {
	return control.Result{Kind: control.Conflict}
}
func (externalPolicy) History(context.Context) control.Result {
	return control.Result{Kind: control.Success}
}
func (externalPolicy) HistoryAction(context.Context, control.HistoryRequest, string) control.Result {
	return control.Result{Kind: control.NotFound, Events: []control.Event{{Action: "preview"}}}
}
func TestExternalPolicyContract(t *testing.T) {
	_ = control.Update{Document: control.Document{SchemaVersion: 1}}
	var kind control.Kind = control.Unavailable
	h := &control.Handler{Service: externalPolicy{}}
	w := httptest.NewRecorder()
	h.Policy(w, httptest.NewRequest("GET", "/", nil))
	if kind != control.Unavailable || w.Code != 503 {
		t.Fatal(w.Code)
	}
}
