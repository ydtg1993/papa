package app

import (
	"os"
	"strings"
	"testing"
)

// Guard the shutdown contract at the application composition boundary. Pool
// tests exercise actual process cleanup; this ensures timeout also reaches it.
func TestShutdownClosesBrowserPoolBeforeDrainedDatabaseGuard(t *testing.T) {
	data, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	start := strings.Index(source, "func (a *App) Run(")
	if start < 0 {
		t.Fatal("Run entry point missing")
	}
	end := strings.Index(source[start:], "func (a *App) Shutdown(")
	if start < 0 || end < 0 {
		t.Fatal("shutdown entry points missing")
	}
	run := source[start : start+end]
	closePool := strings.Index(run, "if pool := a.Engine.GetBrowserPool(); pool != nil {")
	closeDB := strings.Index(run, "if drained {")
	if closePool < 0 || closeDB < 0 || closePool > closeDB {
		t.Fatal("browser cleanup must run independently of drain success")
	}
	if !strings.Contains(run[closePool:closeDB], "pool.Close()") {
		t.Fatal("pool is not closed before database guard")
	}
}
