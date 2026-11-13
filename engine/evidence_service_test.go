package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/config"
)

func TestArchiveServiceIndependentWriteAndCleanup(t *testing.T) {
	root := t.TempDir()
	s := archiveService{options: config.ArchiveConfig{Enabled: true, Dir: root}, logger: logrus.New()}
	task := &Task{ID: 7, Site: "../site", Stage: "stage/../../", Retry: 2}
	buffer := s.newBuffer(task, 3)
	buffer.holdRendered("<noscript>raw</noscript>", "https://example.test/page")
	files := buffer.finish(true)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	path := filepath.Join(root, files[0])
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("escaped root: %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "<noscript>raw</noscript>" {
		t.Fatalf("body = %q, %v", data, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.cleanup(ctx, time.Hour); got != 0 {
		t.Fatalf("cancelled cleanup deleted %d", got)
	}
	if got := s.cleanup(context.Background(), time.Hour); got != 1 {
		t.Fatalf("cleanup deleted %d", got)
	}
	if _, err := os.Stat(strings.TrimSuffix(path, ".html") + ".json"); err != nil {
		t.Fatalf("fresh metadata removed: %v", err)
	}
}

func TestEvidenceFacadeBareEngineAndFreshConfig(t *testing.T) {
	e := &Engine{cfg: &config.Config{}}
	if e.newTrace(nil, 0) != nil || e.newArchiveBuffer(nil, 0) != nil {
		t.Fatal("disabled recording must be a no-op")
	}
	e.cfg.Crawler.Trace.Mode = config.TraceOff
	if _, err := e.ListTraceExecution(7); err == nil {
		t.Fatal("disabled list should report configuration error")
	}
	if e.traceRetention() != defaultTraceRetention || e.archiveRetention() != defaultArchiveRetention {
		t.Fatal("zero defaults changed")
	}
	e.cfg.Crawler.Trace.Retention = -1
	e.cfg.Crawler.Archive.Retention = 2 * time.Hour
	e.cfg.Crawler.Archive.MaxFileMB = 3
	if e.traceRetention() != 0 || e.archiveRetention() != 2*time.Hour || e.archiveMaxBytes() != 3<<20 {
		t.Fatal("facade cached stale configuration")
	}
}

func TestEvidenceMaintenanceEngineStopDrainsCleanup(t *testing.T) {
	e, pool := urgentEngine(t, newFakeTaskDB())
	e.ctx, e.cancel = context.WithCancel(context.Background())
	pool.Start(context.Background(), func(context.Context, *Task) error { return nil })
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	startEvidenceMaintenance(e.ctx, &e.tickerWG, time.Millisecond, func(ctx context.Context) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not start")
	}
	if drained, _ := e.Stop(20 * time.Millisecond); drained {
		t.Fatal("Stop returned drained while cleanup was blocked")
	}
	unblock()
	if drained, _ := e.Stop(time.Second); !drained {
		t.Fatal("Stop did not drain completed cleanup")
	}
}
