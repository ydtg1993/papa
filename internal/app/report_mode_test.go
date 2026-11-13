package app

import (
	"github.com/ydtg1993/papa/v4/obs/report"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOnlineReportNeverScansHistoricalFiles(t *testing.T) {
	a := signalReportApp(t, 0, true)
	if err := os.MkdirAll(a.Config.Log.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	// Malformed on-disk input cannot affect the independent signal consumer.
	if err := os.WriteFile(filepath.Join(a.Config.Log.Dir, "events.jsonl"), []byte("not JSON\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.Events().Emit(events.Event{Type: events.TypeTaskFinished, TS: time.Now().AddDate(0, 0, -1)})
	a.closeEvents()
	m := signalReportJSON(t, a)
	if m["mode"] != "snapshot" {
		t.Fatal(m["mode"])
	}
	if _, err := os.Stat(filepath.Join(a.Config.Obs.Report.Dir, report.DayName(time.Now().AddDate(0, 0, -1)))); !os.IsNotExist(err) {
		t.Fatal("online rebuilt history", err)
	}
}
