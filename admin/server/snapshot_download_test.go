package server

import (
	"context"
	"net/http"
	"testing"
)

func TestTaskSnapshotCallbackComposition(t *testing.T) {
	calls := 0
	cfg := MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, TaskTraceSnapshot: func(ctx context.Context, token string) ([]byte, error) {
		calls++
		if ctx == nil || token != "opaque-token" {
			t.Fatalf("context=%v token=%q", ctx, token)
		}
		return []byte("{\"raw\":true}\x00"), nil
	}}
	rr := serve(NewMonitor(fakeStageStats, testLogger{t}, cfg), http.MethodGet, "/api/task/page?file=opaque-token", nil)
	if rr.Code != 200 || calls != 1 || rr.Body.String() != "{\"raw\":true}\x00" {
		t.Fatalf("status=%d calls=%d body=%q", rr.Code, calls, rr.Body.String())
	}
	if rr.Header().Get("Content-Type") != "application/octet-stream" || rr.Header().Get("Content-Disposition") != `attachment; filename="snapshot.bin"` {
		t.Fatalf("headers=%v", rr.Header())
	}
}
