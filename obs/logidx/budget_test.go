package logidx

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestBuildContextBoundsOutsideWindowAndMalformedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "engine.log")
	raw := []byte("broken\ntime=\"2026-10-24 12:00:00\" level=info msg=\"a\"\n")
	os.WriteFile(p, raw, 0600)
	cfg := Config{Paths: []string{p}, From: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, n := range []int64{int64(len(raw)), int64(len(raw) - 1)} {
		idx, err := BuildContext(context.Background(), cfg, inputbudget.Limits{MaxBytes: n})
		if (err == nil) != (n == int64(len(raw))) {
			t.Fatal(err)
		}
		if err != nil && !errors.Is(err, inputbudget.ErrLimit) {
			t.Fatal(err)
		}
		if err != nil && idx != nil {
			t.Fatal("partial index")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BuildContext(ctx, cfg, inputbudget.Limits{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = BuildContext(context.Background(), cfg, inputbudget.Limits{MaxLineBytes: 3})
	if !errors.Is(err, inputbudget.ErrLimit) {
		t.Fatal(err)
	}
}
func TestBuildWindowsMatchesIndependentTruncationAndBadLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.log")
	q := filepath.Join(dir, "b.log")
	os.WriteFile(p, []byte("bad\ntime=\"2026-10-24 12:00:00\" level=info msg=\"a\"\ntime=\"2026-10-25 00:00:00\" level=info msg=\"b\"\nbad\n"), 0600)
	os.WriteFile(q, []byte("time=\"2026-10-25 12:00:00\" level=info msg=\"c\"\nbad\n"), 0600)
	d := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	windows := []Window{{d, d.Add(24 * time.Hour)}, {d.Add(24 * time.Hour), d.Add(48 * time.Hour)}}
	cfg := Config{Paths: []string{p, q}, MaxEntries: 1}
	got, err := BuildWindowsContext(context.Background(), cfg, windows, inputbudget.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range windows {
		single := cfg
		single.From = w.From
		single.To = w.To
		want, err := Build(single)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[i], want) {
			t.Fatalf("window %d got=%+v want=%+v", i, got[i], want)
		}
	}
}
