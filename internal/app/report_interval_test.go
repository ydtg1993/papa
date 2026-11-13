package app

import (
	"github.com/ydtg1993/papa/v4/obs/report"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSignalPeriodicReportsStopWithApp(t *testing.T) {
	a := signalReportApp(t, 20*time.Millisecond, true)
	a.Events().Emit(events.Event{Type: events.TypeRunStarted})
	path := filepath.Join(a.Config.Obs.Report.Dir, report.DayName(time.Now()), "report.json")
	until := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("periodic report missing")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.closeEvents()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a.reportRunner.Start()
	a.reportRunner.Periodic()
	time.Sleep(50 * time.Millisecond)
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("closed reporter restarted", err)
	}
}
