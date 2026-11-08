package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/agentadmin"
	"github.com/ydtg1993/papa/v4/pkg/agentstore"
	"github.com/ydtg1993/papa/v4/pkg/crawlagent"
)

type activityAdmin struct {
	AgentAdministration
	page               agentadmin.RunPage
	detail             agentadmin.RunDetail
	listErr, detailErr error
	filter             agentstore.RunFilter
	requested          string
	bounded            bool
}

func (a *activityAdmin) Runs(ctx context.Context, f agentstore.RunFilter) (agentadmin.RunPage, error) {
	a.filter = f
	d, ok := ctx.Deadline()
	a.bounded = ok && time.Until(d) <= 2*time.Second
	return a.page, a.listErr
}
func (a *activityAdmin) Run(ctx context.Context, id string, after uint64) (agentadmin.RunDetail, error) {
	a.requested = id
	return a.detail, a.detailErr
}
func TestMonitorAgentActivityBoundedDurableSnapshot(t *testing.T) {
	now := time.Now()
	a := &activityAdmin{page: agentadmin.RunPage{Items: []agentadmin.RunView{
		{ID: "z-old", CreatedAt: now.Add(-time.Hour)}, {ID: "a-new", CreatedAt: now},
	}, Next: "cursor"}, detail: agentadmin.RunDetail{Run: agentadmin.RunView{ID: "a-new", Status: "awaiting_review", Usage: crawlagent.Usage{ModelCalls: 12, InputTokens: 50, OutputTokens: 4}}}}
	s := NewMonitor(fakeStageStats, testLogger{t}, MonitorConfig{AgentAdmin: a})
	out := s.agentActivity(context.Background())
	if !a.bounded || a.filter.Validate() != nil || a.filter.Until.Sub(a.filter.From) != 24*time.Hour || a.requested != "a-new" || !out.Partial || out.Runs[0].Status != "awaiting_review" || out.Runs[0].Usage.ModelCalls != 12 {
		t.Fatalf("bad snapshot: %+v, %+v", out, a)
	}
	// Audit failure is explicit and does not break the independent queue response
	// or expose a database error/credential through the monitor endpoint.
	a.listErr = errors.New("secret database connection error")
	w := httptest.NewRecorder()
	s.apiHandler(w, httptest.NewRequest("GET", "/api/monitor", nil))
	var response map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response["stages"] == nil || !strings.Contains(w.Body.String(), "audit_unavailable") || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body.String())
	}
	a.listErr = nil
	a.detailErr = errors.New("secret")
	out = s.agentActivity(context.Background())
	if out.Error != "steps_unavailable" || len(out.Runs) != 2 || out.Detail != nil {
		t.Fatalf("%+v", out)
	}
	a.page = agentadmin.RunPage{}
	a.requested = ""
	out = s.agentActivity(context.Background())
	if out.Error != "" || a.requested != "" {
		t.Fatalf("empty list read detail: %+v", out)
	}
}
