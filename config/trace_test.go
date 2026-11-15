package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceModeLoad(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       TraceMode
	}{
		{"default", "", TraceErrors}, {"off", "mode: off", TraceOff}, {"all", "mode: all", TraceAll}, {"errors", "mode: errors", TraceErrors},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			body := "log: {dir: ./logs}\ndb: {max_idle_conns: 10, max_open_conns: 100}\ncrawler:\n  trace:\n    " + tc.body + "\n"
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(p)
			if err != nil {
				t.Fatal(err)
			}
			if c.Crawler.Trace.Mode != tc.want {
				t.Fatalf("got %+v", c.Crawler.Trace)
			}
		})
	}
}
func TestTraceModeInvalid(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("invalid mode accepted")
		}
	}()
	_ = (TraceConfig{Mode: "typo"}).ModeOrDefault()
}
func TestTraceModeGoZeroDefaultsToErrors(t *testing.T) {
	if (TraceConfig{}).ModeOrDefault() != TraceErrors {
		t.Fatal("wrong default")
	}
}

func TestTraceRejectsRemovedEnabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("crawler:\n  trace:\n    enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("removed configuration accepted")
		}
	}()
	_, _ = Load(p)
}

func TestTraceRejectsRemovedArchive(t *testing.T) {
	for _, body := range []string{"archive: {enabled: false}", "archive: {mode: always, dir: ./old, retention: 168h, max_file_mb: 8}"} {
		t.Run(body, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte("crawler:\n  "+body+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			defer func() {
				got := fmt.Sprint(recover())
				for _, hint := range []string{"crawler.archive", "crawler.trace.mode", "crawler.trace_files"} {
					if !strings.Contains(got, hint) {
						t.Fatalf("missing migration hint %q: %s", hint, got)
					}
				}
			}()
			_, _ = Load(p)
		})
	}
}
