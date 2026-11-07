package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/admin/sysinfo"
)

func TestMonitorObservationMetadataIsStablePerInstance(t *testing.T) {
	monitor := newFakeMonitor(t)
	var identity string
	for i := 0; i < 2; i++ {
		before := time.Now().UTC()
		response := serve(monitor, http.MethodGet, "/api/monitor", nil)
		after := time.Now().UTC()
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
		body := decodeJSON(t, response)
		id, ok := body["instance_id"].(string)
		if !ok || id == "" {
			t.Fatalf("missing monitor instance identity: %v", body["instance_id"])
		}
		if i == 0 {
			identity = id
		} else if id != identity {
			t.Fatalf("same monitor changed identity: %q -> %q", identity, id)
		}
		raw, ok := body["generated_at"].(string)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if !ok || err != nil || !strings.HasSuffix(raw, "Z") || at.Before(before) || at.After(after) {
			t.Fatalf("generated_at must be request-time UTC RFC3339Nano: %v (%v), bounds=[%v,%v]", body["generated_at"], err, before, after)
		}
	}
	other := decodeJSON(t, serve(newFakeMonitor(t), http.MethodGet, "/api/monitor", nil))
	if other["instance_id"] == identity {
		t.Fatal("new monitor reused the previous observation identity")
	}
}

func TestMonitorUnsampledSystemMetadataDoesNotClaimZeroSuccess(t *testing.T) {
	monitor := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, SysInfo: sysinfo.NewCollector(30*time.Second, nil)})
	body := decodeJSON(t, serve(monitor, http.MethodGet, "/api/monitor", nil))
	system, ok := body["system"].(map[string]any)
	if !ok {
		t.Fatalf("missing system snapshot: %v", body)
	}
	for _, metric := range []string{"cpu", "mem", "disk"} {
		if system[metric+"_available"] != false {
			t.Errorf("unsampled %s reported available: %v", metric, system)
		}
		if value, present := system[metric+"_sampled_at"]; !present || value != nil {
			t.Errorf("unsampled %s timestamp = %v (present=%v), want null", metric, value, present)
		}
	}
	if system["sample_interval_ms"] != float64(30000) {
		t.Fatalf("sample_interval_ms = %v, want 30000", system["sample_interval_ms"])
	}
	for _, key := range []string{"cpu_percent", "mem_used", "mem_total", "mem_percent", "disk_used", "disk_total", "disk_percent", "dirs"} {
		if _, present := system[key]; !present {
			t.Errorf("existing system field %s was removed", key)
		}
	}
}
