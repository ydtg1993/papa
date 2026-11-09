package app

import (
	"context"
	"github.com/ydtg1993/papa/v4/config"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBrowserDiagnosticLaunchFailureAndCleanup(t *testing.T) {
	a := &App{Config: &config.Config{}}
	a.Config.Browser.Enable = true
	a.Config.Browser.PoolSize = 1
	a.Config.Browser.Binary = "path"
	a.Config.Browser.BrowserPath = "nonexistent-diagnostic-browser"
	a.Config.Browser.TempDir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report := runRodDiagnostic(ctx, a)
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
	got := redactRod("https://user:pass@host/path?token=secret password=hidden", nil)
	for _, secret := range []string{"user:pass", "secret", "hidden"} {
		if strings.Contains(got, secret) {
			t.Fatal(got)
		}
	}
}
