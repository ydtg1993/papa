package monitoring

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/core"
	"net/http/httptest"
	"testing"
)

func TestSnapshotIdentityAndStableBreakerOrder(t *testing.T) {
	cfg := Options{Stages: func() map[string]core.StageStats {
		return map[string]core.StageStats{"site:detail": {Stage: "detail", Site: "site"}}
	}, BreakerStatuses: func() map[string]core.BreakerStatus { return map[string]core.BreakerStatus{"z": {}, "a": {}} }}
	h := New(cfg)
	snapshot := func(h *Handler) map[string]any {
		w := httptest.NewRecorder()
		h.Snapshot(w, httptest.NewRequest("GET", "/api/monitor", nil))
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	a, b, c := snapshot(h), snapshot(h), snapshot(New(cfg))
	if a["instance_id"] != b["instance_id"] || a["instance_id"] == c["instance_id"] {
		t.Fatal("identity must belong to component lifecycle")
	}
	if a["breakers"].([]any)[0].(map[string]any)["site"] != "a" {
		t.Fatal(a)
	}
}
