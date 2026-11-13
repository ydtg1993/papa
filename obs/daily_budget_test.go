package obs

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConsumeDailySourcesEquivalentIndependentDays(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	d := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	writeEventFile(t, p, []string{eventLine(1, d, "r", "x"), eventLine(2, d.AddDate(0, 0, 1), "r", "x"), eventLine(1, d.AddDate(0, 0, 1), "r", "x"), eventLine(1, d, "r", "x"), "bad", eventLine(3, d, "other", "x"), eventLine(3, d, "other", "x")})
	os.WriteFile(filepath.Join(dir, "engine.log"), []byte("broken\ntime=\"2026-10-24 12:00:00\" level=info msg=\"a\"\ntime=\"2026-10-25 00:00:00\" level=info msg=\"b\"\n"), 0600)
	for _, run := range []string{"", "r"} {
		sc := SourceConfig{EventsFile: p, LogDir: dir, Loc: time.UTC, RunID: run, IncludeLogs: true}
		count := 0
		err := ConsumeDailySources(context.Background(), sc, inputbudget.Limits{}, func(got *Sources) error {
			single := sc
			single.Since = got.Run.From
			single.Until = got.Run.To
			want, e := LoadSources(single)
			if e != nil {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("daily mismatch\ngot=%+v\nwant=%+v", got, want)
			}
			count++
			return nil
		})
		if err != nil || count != 2 {
			t.Fatalf("count=%d err=%v", count, err)
		}
	}
}
func TestConsumeDailySourcesPreflightPreservesOutput(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	out := filepath.Join(dir, "report.json")
	os.WriteFile(out, []byte("prior"), 0600)
	d := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	writeEventFile(t, p, []string{eventLine(1, d, "r", "x"), eventLine(2, d.AddDate(0, 0, 1), "r", strings.Repeat("x", 200))})
	for _, limits := range []inputbudget.Limits{{MaxRetainedBytes: 160}, {MaxSpoolBytes: 20}, {MaxDays: 1}, {MaxDedupKeys: 1}} {
		calls := 0
		err := ConsumeDailySources(context.Background(), SourceConfig{EventsFile: p, Loc: time.UTC}, limits, func(*Sources) error { calls++; return os.WriteFile(out, []byte("new"), 0600) })
		data, _ := os.ReadFile(out)
		if !errors.Is(err, inputbudget.ErrLimit) || calls != 0 || string(data) != "prior" {
			t.Fatalf("err=%v calls=%d output=%s", err, calls, data)
		}
	}
}
func TestConsumeDailySourcesCancellation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	d := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	writeEventFile(t, p, []string{eventLine(1, d, "r", "x"), eventLine(2, d.AddDate(0, 0, 1), "r", "x")})
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := ConsumeDailySources(ctx, SourceConfig{EventsFile: p}, inputbudget.Limits{}, func(*Sources) error { calls++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("%v calls=%d", err, calls)
	}
}

func TestConsumeDailySourcesExactUnterminatedLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	raw := eventLine(1, time.Now(), "r", "x")
	os.WriteFile(p, []byte(raw), 0600)
	calls := 0
	err := ConsumeDailySources(context.Background(), SourceConfig{EventsFile: p}, inputbudget.Limits{MaxLineBytes: int64(len(raw)), MaxBytes: int64(len(raw))}, func(*Sources) error { calls++; return nil })
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
