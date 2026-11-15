package browserdiagnostics

import (
	"context"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/ydtg1993/papa/v4/config"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBrowserDiagnosticLaunchFailureAndCleanup(t *testing.T) {
	cfg := &config.Config{}
	cfg.Browser.Enable = true
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

// The helper never announces a DevTools endpoint, exercising cancellation
// during startup rather than the normal successful browser-close path.
func init() {
	if path := os.Getenv("PAPA_ROD_DIAGNOSTIC_HANG_PID"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func TestRodDiagnosticCancellationReapsStartingProcess(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pidFile := filepath.Join(root, "helper.pid")
	t.Setenv("PAPA_ROD_DIAGNOSTIC_HANG_PID", pidFile)
	cfg := &config.Config{}
	cfg.Browser.Enable = true
	cfg.Browser.Binary = "path"
	cfg.Browser.BrowserPath = bin
	cfg.Browser.TempDir = filepath.Join(root, "profiles")
	cfg.Browser.Leakless = false
	a := &Service{}
	a.Configure(Options{Config: &cfg.Browser})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan RodReport, 1)
	go func() { done <- a.runRodDiagnostic(ctx) }()
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(string(data))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("helper did not start")
	}
	defer func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}()
	cancel()
	select {
	case report := <-done:
		if report.OK {
			t.Fatal("cancelled diagnostic reported success")
		}
		exists, err := process.PidExists(int32(pid))
		if err != nil || exists {
			t.Fatalf("helper survived: pid=%d exists=%v err=%v", pid, exists, err)
		}
		dir, _ := report.Environment["instance_temp_dir"].(string)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("profile retained: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("diagnostic cancellation did not drain")
	}
}

func TestRodDiagnosticRealBrowserCleanupWithoutLeakless(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("system browser unavailable")
	}
	cfg := &config.Config{}
	cfg.Browser.Enable = true
	cfg.Browser.Binary = "system"
	cfg.Browser.Headless = true
	cfg.Browser.NoSandbox = true
	cfg.Browser.TempDir = t.TempDir()
	a := &Service{}
	a.Configure(Options{Config: &cfg.Browser})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report := a.runRodDiagnostic(ctx)
	if !report.OK {
		t.Fatalf("%+v", report)
	}
	dir, _ := report.Environment["instance_temp_dir"].(string)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("diagnostic profile retained: %v", err)
	}
}
