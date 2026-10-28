package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

// 事件是毫秒精度、日志是秒精度：窗口边界同一秒的日志必须被捞到（±5 分钟冗余）。
func TestLoadSourcesIncludesBoundaryLogs(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 28, 15, 28, 50, 547_000_000, time.Local)
	ev := events.Event{V: 1, Seq: 1, TS: start, RunID: "r-1", Type: events.TypeRunStarted}

	evFile, err := os.Create(filepath.Join(logDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(evFile).Encode(ev); err != nil {
		t.Fatal(err)
	}
	if err := evFile.Close(); err != nil {
		t.Fatal(err)
	}

	logLine := "time=\"" + start.Format("2006-01-02 15:04:05") + "\" level=info msg=\"boom\"\n"
	if err := os.WriteFile(filepath.Join(logDir, "engine.log"), []byte(logLine), 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := LoadSources(SourceConfig{
		EventsFile:  filepath.Join(logDir, "events.jsonl"),
		LogDir:      logDir,
		IncludeLogs: true,
		Loc:         time.Local,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Logs.Signatures) == 0 {
		t.Fatalf("窗口边界同一秒的日志应被索引到：logs=%+v", src.Logs)
	}
}
