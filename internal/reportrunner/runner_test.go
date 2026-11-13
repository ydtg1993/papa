package reportrunner

import (
	"context"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/obs"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type quietLogger struct{}

func (quietLogger) Infof(string, ...any) {}
func (quietLogger) Warnf(string, ...any) {}

type blockingSection struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingSection) Key() string   { return "blocking" }
func (*blockingSection) Title() string { return "blocking" }
func (s *blockingSection) Build(ctx context.Context, _ *obs.Input) (*obs.SectionResult, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return &obs.SectionResult{}, nil
	case <-ctx.Done():
		// Cancellation requests cleanup; Stop must still wait for it.
		<-s.release
		return nil, ctx.Err()
	}
}
func TestStopWaitsForReportBeforeStorageClose(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{}
	cfg.LogDir = dir
	cfg.Enabled = true
	cfg.Interval = time.Millisecond
	cfg.Timeout = 5 * time.Second
	cfg.ReportDir = filepath.Join(dir, "reports")
	cfg.Retention = -1
	raw, err := json.Marshal(map[string]any{"v": 1, "seq": 1, "ts": time.Now().Format(time.RFC3339Nano), "run_id": "fixture", "type": "run_started"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "events.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	section := &blockingSection{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(Options{Config: cfg, Logger: quietLogger{}, Sections: []obs.Section{section}})
	r.Start()
	r.Start()
	select {
	case <-section.entered:
	case <-time.After(3 * time.Second):
		close(section.release)
		r.Stop()
		t.Fatal("report did not start")
	}
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned while report still uses storage")
	case <-time.After(20 * time.Millisecond):
	}
	close(section.release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not drain")
	}
	r.Stop()
}
func TestDisabledRunnerNeedsNoDependencies(t *testing.T) {
	r := New(Options{})
	r.Start()
	r.Stop()
	r.Exit()
	r.Periodic()
}

func TestRunnerOwnsConfigurationSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Enabled: true, LogDir: dir, ReportDir: filepath.Join(dir, "reports"), Interval: time.Hour, Retention: -1}
	raw, err := json.Marshal(map[string]any{"v": 1, "seq": 1, "ts": time.Now().Format(time.RFC3339Nano), "run_id": "fixture", "type": "run_started"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(Options{Config: cfg, Logger: quietLogger{}})
	cfg.Enabled = false
	cfg.ReportDir = filepath.Join(dir, "changed")
	r.Periodic()
	files, err := filepath.Glob(filepath.Join(dir, "reports", "*", "report.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("runner did not use its configuration snapshot: files=%v err=%v", files, err)
	}
	if _, err := os.Stat(cfg.ReportDir); !os.IsNotExist(err) {
		t.Fatalf("runner used caller's changed configuration: %v", err)
	}
}

func TestRunnerInputLimitPreservesPriorReport(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().Format("2006-01-02")
	target := filepath.Join(dir, "reports", day)
	os.MkdirAll(target, 0700)
	prior := filepath.Join(target, "report.json")
	os.WriteFile(prior, []byte("prior"), 0600)
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("over budget malformed line\n"), 0600)
	r := NewWithLimits(Options{Config: Config{Enabled: true, LogDir: dir, ReportDir: filepath.Join(dir, "reports"), Retention: time.Nanosecond}, Logger: quietLogger{}}, inputbudget.Limits{MaxLineBytes: 4})
	r.Generate(obs.TriggerPeriodic)
	got, err := os.ReadFile(prior)
	if err != nil || string(got) != "prior" {
		t.Fatalf("output=%s err=%v", got, err)
	}
}
