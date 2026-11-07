package sysinfo

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

func successfulSources() systemSources {
	return systemSources{
		cpuPercent: func(time.Duration, bool) ([]float64, error) { return []float64{0}, nil },
		memory: func() (*mem.VirtualMemoryStat, error) {
			return &mem.VirtualMemoryStat{Total: 1024}, nil
		},
		diskUsage: func(string) (*disk.UsageStat, error) { return &disk.UsageStat{Total: 2048}, nil },
		now: func() time.Time {
			return time.Date(2026, 11, 7, 8, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
		},
	}
}

func TestSystemSamplingDistinguishesUnsampledFromZero(t *testing.T) {
	c := newCollector(30*time.Second, nil, successfulSources())
	before := c.Snapshot()
	if before.CPUAvailable || before.MemAvailable || before.DiskAvailable || before.CPUSampledAt != nil || before.MemSampledAt != nil || before.DiskSampledAt != nil {
		t.Fatalf("unsampled metrics must be unavailable with no success time: %+v", before)
	}
	if before.SampleIntervalMS != 30000 {
		t.Fatalf("sample interval = %d, want 30000 ms", before.SampleIntervalMS)
	}
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"cpu_sampled_at", "mem_sampled_at", "disk_sampled_at"} {
		if value, present := fields[key]; !present || value != nil {
			t.Errorf("unsampled JSON %s = %v (present=%v), want null", key, value, present)
		}
	}
	c.collectSystem()
	after := c.Snapshot()
	if !after.CPUAvailable || !after.MemAvailable || !after.DiskAvailable || after.CPUPercent != 0 || after.MemUsed != 0 || after.DiskUsed != 0 {
		t.Fatalf("successful zero measurements are available: %+v", after)
	}
	want := successfulSources().now().UTC()
	for _, at := range []*time.Time{after.CPUSampledAt, after.MemSampledAt, after.DiskSampledAt} {
		if at == nil || !at.Equal(want) || at.Location() != time.UTC {
			t.Errorf("sample completion time = %v, want UTC %v", at, want)
		}
	}
	raw, err = json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["cpu_sampled_at"] != "2026-11-07T00:00:00.123456789Z" {
		t.Fatalf("successful sample timestamp wire format = %v", fields["cpu_sampled_at"])
	}
	// Timestamps exposed by Snapshot must not let callers mutate collector state.
	*after.CPUSampledAt = time.Time{}
	if !c.Snapshot().CPUSampledAt.Equal(want) {
		t.Fatal("modifying a returned sample timestamp mutated the collector")
	}
}

func TestSystemSamplingFailureRetainsLastSuccessButMarksUnavailable(t *testing.T) {
	failing := false
	sources := successfulSources()
	sources.cpuPercent = func(time.Duration, bool) ([]float64, error) {
		if failing {
			return nil, errors.New("CPU unavailable")
		}
		return []float64{25}, nil
	}
	sources.memory = func() (*mem.VirtualMemoryStat, error) {
		if failing {
			return nil, errors.New("memory unavailable")
		}
		return &mem.VirtualMemoryStat{Used: 256, Total: 1024, UsedPercent: 25}, nil
	}
	sources.diskUsage = func(string) (*disk.UsageStat, error) {
		if failing {
			return nil, errors.New("disk unavailable")
		}
		return &disk.UsageStat{Used: 1024, Total: 2048, UsedPercent: 50}, nil
	}
	c := newCollector(time.Second, nil, sources)
	c.collectSystem()
	before := c.Snapshot()
	failing = true
	c.collectSystem()
	after := c.Snapshot()
	if after.CPUAvailable || after.MemAvailable || after.DiskAvailable {
		t.Fatalf("failed measurements must be unavailable: %+v", after)
	}
	if after.CPUPercent != 25 || after.MemUsed != 256 || after.DiskUsed != 1024 || !after.CPUSampledAt.Equal(*before.CPUSampledAt) || !after.MemSampledAt.Equal(*before.MemSampledAt) || !after.DiskSampledAt.Equal(*before.DiskSampledAt) {
		t.Fatalf("failed sample lost last successful values/times: before=%+v after=%+v", before, after)
	}
	failing = false
	c.collectSystem()
	if recovered := c.Snapshot(); !recovered.CPUAvailable || !recovered.MemAvailable || !recovered.DiskAvailable {
		t.Fatalf("successful sampling did not recover availability: %+v", recovered)
	}
}

func TestSystemSamplingAvailabilityIsIndependentAndRejectsInvalidValues(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		sources := successfulSources()
		sources.cpuPercent = func(time.Duration, bool) ([]float64, error) { return []float64{bad}, nil }
		sources.diskUsage = func(string) (*disk.UsageStat, error) { return nil, nil }
		c := newCollector(time.Second, nil, sources)
		c.collectSystem()
		snapshot := c.Snapshot()
		if snapshot.CPUAvailable || !snapshot.MemAvailable || snapshot.DiskAvailable || snapshot.CPUSampledAt != nil || snapshot.MemSampledAt == nil || snapshot.DiskSampledAt != nil {
			t.Fatalf("availability must reflect each source independently for %v: %+v", bad, snapshot)
		}
		if _, err := json.Marshal(snapshot); err != nil {
			t.Fatalf("invalid sample escaped into monitor JSON: %v", err)
		}
	}
}

func TestSystemSamplingDoesNotBlockSnapshotOrOverwriteDirs(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	sources := successfulSources()
	sources.cpuPercent = func(time.Duration, bool) ([]float64, error) {
		close(entered)
		<-release
		return []float64{20}, nil
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newCollector(time.Second, map[string]string{"logs": dir}, sources)
	go func() { c.collectSystem(); close(done) }()
	stop := sync.OnceFunc(func() { close(release); <-done })
	defer stop()
	<-entered
	read := make(chan Snapshot, 1)
	go func() { read <- c.Snapshot() }()
	select {
	case snapshot := <-read:
		if snapshot.CPUAvailable {
			t.Fatal("unfinished CPU measurement was published")
		}
	case <-time.After(time.Second):
		t.Fatal("Snapshot blocked behind slow CPU sampling")
	}
	// A directory publication while CPU is blocked must survive the system merge.
	c.collectDirs()
	stop()
	snapshot := c.Snapshot()
	if !snapshot.CPUAvailable || snapshot.CPUPercent != 20 || snapshot.Dirs["logs"].Size != 6 {
		t.Fatalf("system publication replaced concurrent directory data: %+v", snapshot)
	}
}

func TestSystemSamplingNormalizesNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		c := newCollector(interval, nil, successfulSources())
		if c.interval != 2*time.Second || c.Snapshot().SampleIntervalMS != 2000 {
			t.Fatalf("invalid interval %v was not normalized: interval=%v snapshot=%+v", interval, c.interval, c.Snapshot())
		}
	}
}

func TestCollectorSnapshotDirsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newCollector(time.Second, map[string]string{"logs": dir}, successfulSources())
	c.collectDirs()
	first := c.Snapshot()
	second := c.Snapshot()
	first.Dirs["logs"] = DirStat{Path: "caller-owned", Size: -1}
	first.Dirs["extra"] = DirStat{}
	for _, snapshot := range []Snapshot{second, c.Snapshot()} {
		if len(snapshot.Dirs) != 1 || snapshot.Dirs["logs"].Path != dir || snapshot.Dirs["logs"].Size != 6 {
			t.Fatalf("modifying returned directory stats contaminated another snapshot: %+v", snapshot.Dirs)
		}
	}
}

func TestCollectorOwnsDirectoryConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{"logs": dir}
	c := newCollector(time.Second, dirs, successfulSources())
	dirs["logs"] = t.TempDir()
	dirs["extra"] = dir
	c.collectDirs()
	snapshot := c.Snapshot()
	if len(snapshot.Dirs) != 1 || snapshot.Dirs["logs"].Path != dir || snapshot.Dirs["logs"].Size != 6 {
		t.Fatalf("caller directory config changes contaminated the collector: %+v", snapshot.Dirs)
	}
}
