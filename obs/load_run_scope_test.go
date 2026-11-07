package obs_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/analyze"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

func writeRunScopeEvents(t *testing.T, name string, compressed bool, evs ...events.Event) {
	t.Helper()
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	for _, ev := range evs {
		if err := encoder.Encode(ev); err != nil {
			t.Fatal(err)
		}
	}
	data := body.Bytes()
	if compressed {
		var packed bytes.Buffer
		writer := gzip.NewWriter(&packed)
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		data = packed.Bytes()
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSourcesGapCountFollowsRunAndWindow(t *testing.T) {
	base := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	ev := func(run string, seq uint64, minute int) events.Event {
		return events.Event{V: events.Version, RunID: run, Seq: seq, TS: base.Add(time.Duration(minute) * time.Minute), Type: events.TypeQueueRound}
	}
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	writeRunScopeEvents(t, filepath.Join(dir, "events-2026-11-03T10-01-00.000.jsonl.gz"), true,
		ev("C", 10, 0), ev("A", 2, 1), ev("B", 1, 0), ev("A", 1, 0))
	writeRunScopeEvents(t, active, false,
		ev("B", 4, 2), ev("A", 3, 2), ev("C", 12, 2), ev("A", 2, 1))
	for _, tc := range []struct {
		name, run   string
		since       time.Time
		gaps, count int
	}{
		{"healthy-A", "A", time.Time{}, 0, 3},
		{"gapped-B", "B", time.Time{}, 2, 2},
		{"gapped-C-starts-at-ten", "C", time.Time{}, 1, 2},
		{"all-runs", "", time.Time{}, 3, 7},
		{"B-window-excludes-gap", "B", base.Add(time.Minute), 0, 1},
		{"all-window-excludes-gap", "", base.Add(time.Minute), 0, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := obs.LoadSources(obs.SourceConfig{EventsFile: active, RunID: tc.run, Since: tc.since})
			if err != nil {
				t.Fatal(err)
			}
			if source.EventMeta.Gaps != tc.gaps {
				t.Fatalf("run=%q gaps=%d, want %d", tc.run, source.EventMeta.Gaps, tc.gaps)
			}
			if source.EventMeta.Count != tc.count || len(source.Events) != tc.count {
				t.Fatalf("event count=%d/%d, want %d", source.EventMeta.Count, len(source.Events), tc.count)
			}
			if source.EventMeta.Files != 2 || source.EventMeta.Duplicates != 1 {
				t.Fatal("file-level diagnostics should retain their existing scope")
			}
			if tc.run != "" && (len(source.Run.RunIDs) != 1 || source.Run.RunIDs[0] != tc.run) {
				t.Fatalf("unexpected run IDs: %v", source.Run.RunIDs)
			}
			result, err := (&analyze.Generic{}).Build(context.Background(), &obs.Input{Run: source.Run, EventMeta: source.EventMeta, Events: source.Events, Now: base.Add(3 * time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, check := range result.Checks {
				if check.ID == "event_gaps" {
					found = true
					if check.Passed != (tc.gaps == 0) {
						t.Fatalf("wrong report event_gaps verdict: %+v", check)
					}
				}
			}
			if !found {
				t.Fatal("report omitted event_gaps check")
			}
		})
	}
}
