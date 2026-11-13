package app

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/events"
)

type observationFaultWriter struct {
	entered, release chan struct{}
	once             sync.Once
	panicWrite       bool
}

func (w *observationFaultWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	if w.panicWrite {
		panic("synthetic archive panic")
	}
	<-w.release
	return len(p), nil
}
func (*observationFaultWriter) Close() error { return nil }

// Called before Run has emitted any events. All resources belong to this fixture.
func observationFault(t *testing.T, a *App, fault string) (func(), <-chan struct{}) {
	t.Helper()
	if fault == "report-unwritable" {
		if err := os.WriteFile(a.Config.Obs.Report.Dir, []byte("fixture file blocks directory"), 0600); err != nil {
			t.Fatal(err)
		}
		return func() {}, nil
	}
	if fault != "archive-blocked" && fault != "archive-panic" {
		return func() {}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := a.observation.archive.Close(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	w := &observationFaultWriter{entered: make(chan struct{}), release: make(chan struct{}), panicWrite: fault == "archive-panic"}
	a.observation.archive = events.NewEmitter(a.runID, events.NewJSONLSink(w), 1, time.Now)
	var once sync.Once
	return func() {
		once.Do(func() { close(w.release) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.observation.archive.Close(ctx)
	}, w.entered
}

func TestObservationArchiveCannotHoldAppClose(t *testing.T) {
	// No business DB/Engine is passed to observation, even for its final report.
	a := signalReportApp(t, 0, true)
	release, entered := observationFault(t, a, "archive-blocked")
	defer release()
	a.Events().Emit(events.Event{Type: events.TypeRunStarted})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("archive not entered")
	}
	for i := 0; i < 10000; i++ {
		a.Events().Emit(events.Event{Type: events.TypeTaskFinished})
	}
	start := time.Now()
	a.closeEvents()
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatal("observation exceeded shared close budget")
	}
	if a.observation.archive.Dropped() == 0 {
		t.Fatal("full archive did not report drops")
	}
	_ = signalReportJSON(t, a)
}

func TestAppMySQLObservationIsolation(t *testing.T) {
	for _, fault := range []string{"off", "archive-blocked", "archive-panic", "report-unwritable"} {
		t.Run(fault, func(t *testing.T) {
			t.Setenv("PAPA_APP_OBS_FAULT", fault)
			t.Setenv("PAPA_APP_SATURATION_DURATION", "5s")
			appLoadParent(t, "saturated")
		})
	}
}
