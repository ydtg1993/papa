package app

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/obs/report"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func signalReportApp(t *testing.T, interval time.Duration, enabled bool) *App {
	t.Helper()
	cfg := &config.Config{}
	cfg.Log.Dir = filepath.Join(t.TempDir(), "logs")
	cfg.Obs.Events.Enabled = enabled
	cfg.Obs.Report.OnExit = true
	cfg.Obs.Report.Interval = interval
	cfg.Obs.Report.Dir = filepath.Join(t.TempDir(), "reports")
	a := &App{Config: cfg, runID: "signal-fixture"}
	if enabled {
		a.observation = &observationPipeline{archive: newEventEmitter(cfg, a.runID).(events.Closer)}
		a.events = events.Isolate(a.observation)
	}
	a.startReportLoop()
	t.Cleanup(a.closeEvents)
	return a
}
func signalReportJSON(t *testing.T, a *App) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(a.Config.Obs.Report.Dir, report.DayName(time.Now()), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestSignalExitReportWithoutBusinessResources(t *testing.T) {
	a := signalReportApp(t, 0, true)
	a.Events().Emit(events.Event{Type: events.TypeRunStarted})
	a.Events().Emit(events.Event{Type: events.TypeTaskFinished, Site: "synthetic", Stage: "detail"})
	a.Events().Emit(events.Event{Type: events.TypeRunStopped, Outcome: "drained"})
	a.closeEvents()
	for _, name := range []string{"report.md", "report.json", "ai-brief.md", "ai-brief.json"} {
		b, err := os.ReadFile(filepath.Join(a.Config.Obs.Report.Dir, report.DayName(time.Now()), name))
		if err != nil || len(b) == 0 {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
	m := signalReportJSON(t, a)
	if m["verdict"] != "unknown" {
		t.Fatal("received signals cannot prove all tasks succeeded", m["verdict"])
	}
	// Close is terminal, idempotent and safe after its original budget expired.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.reportRunner.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestSignalReportingDisabledCreatesNoFiles(t *testing.T) {
	a := signalReportApp(t, time.Millisecond, false)
	a.Events().Emit(events.Event{Type: events.TypeTaskFailed})
	a.closeEvents()
	if _, err := os.Stat(a.Config.Obs.Report.Dir); !os.IsNotExist(err) {
		t.Fatal("disabled report wrote files", err)
	}
}
