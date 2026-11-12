package browserdiagnostics

import (
	"context"
	"github.com/ydtg1993/papa/v4/config"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBrowserDiagnosticLaunchFailureAndCleanup(t *testing.T) {
	cfg := &config.Config{}
	cfg.Browser.Enable = true
	cfg.Browser.PoolSize = 1
	cfg.Browser.Binary = "path"
	cfg.Browser.BrowserPath = "nonexistent-diagnostic-browser"
	cfg.Browser.TempDir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := &Service{}
	a.Configure(Options{Config: &cfg.Browser})
	report, err := a.Rod(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.Steps[1].Status != "failed" || report.Steps[2].Status != "skipped" {
		t.Fatalf("%+v", report)
	}
	if dir, ok := report.Environment["instance_temp_dir"].(string); ok {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("profile not cleaned", err)
		}
	}
}

func TestBrowserDiagnosticRedactsSecrets(t *testing.T) {
	got := Redact("https://user:pass@host/path?token=secret password=hidden", nil)
	for _, secret := range []string{"user:pass", "secret", "hidden"} {
		if strings.Contains(got, secret) {
			t.Fatal(got)
		}
	}
}
