package loggers

import (
	"errors"
	"github.com/sirupsen/logrus"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerMasksFileAndConsoleWithoutMutatingFields(t *testing.T) {
	dir := t.TempDir()
	set := NewLoggerSet(LoggerConfig{Dir: dir})
	t.Cleanup(func() { closeLoggers(t, set) })
	fields := logrus.Fields{"nested": map[string]any{"headers": map[string]string{"Cookie": "SECRET"}, "key": "SECRET"}, "error": errors.New("token=SECRET"), "id": uint64(9007199254740993)}
	entry := set.Engine.WithFields(fields)
	entry.Message = "GET https://user:SECRET@example.test/a?sign=SECRET&page=2"
	line, err := entry.String() // consoleHook uses this same formatter
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "SECRET") || !strings.Contains(line, "9007199254740993") {
		t.Fatalf("unsafe/rounded console diagnostic: %s", line)
	}
	entry.Info(entry.Message)
	closeLoggers(t, set)
	raw, err := os.ReadFile(filepath.Join(dir, "engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), "page=2") {
		t.Fatalf("unsafe file diagnostic: %s", raw)
	}
	if !strings.Contains(entry.Message, "SECRET") || fields["nested"].(map[string]any)["key"] != "SECRET" {
		t.Fatal("caller data changed")
	}
}
