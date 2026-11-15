//go:build windows

package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"golang.org/x/sys/windows"
)

func lockDiagnosticProfileFile(t *testing.T) (string, windows.Handle) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "browser-diagnostic-owned")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir, lockDiagnosticFile(t, dir)
}

func lockDiagnosticFile(t *testing.T, dir string) windows.Handle {
	t.Helper()
	name, err := windows.UTF16PtrFromString(filepath.Join(dir, "cache.data"))
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately omit FILE_SHARE_DELETE, like a Chromium cache child which
	// has received termination but has not released its file handle yet.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func TestSelfCheckReturnsProfileCleanupFailure(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("PAPA_DIAGNOSTIC_STARTUP_HELPER", "handshake")
	t.Setenv("PAPA_DIAGNOSTIC_STARTUP_PID", marker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		p, err := NewPoolContext(ctx, PoolConfig{Size: 1, SelfCheck: true, BrowserPath: bin, TempDir: root})
		if p != nil {
			p.Close()
		}
		result <- err
	}()
	waitPool(t, func() bool { _, err := os.Stat(marker + ".connected"); return err == nil })
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing self-check owned profile: %v %v", entries, err)
	}
	handle := lockDiagnosticFile(t, filepath.Join(root, entries[0].Name()))
	defer windows.CloseHandle(handle)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !retryDiagnosticCleanup(err) {
			t.Fatalf("self-check lost startup cancellation or profile cleanup failure: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("self-check cleanup did not finish within its budget")
	}
}

func TestDiagnosticCleanupWaitsForWindowsFileHandleRelease(t *testing.T) {
	dir, handle := lockDiagnosticProfileFile(t)
	defer func() {
		if handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
	}()
	p := &diagnosticProcess{dir: dir}
	result := make(chan error, 1)
	go func() { result <- p.close() }()
	select {
	case err := <-result:
		t.Fatalf("cleanup returned before the owned file handle was released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	handle = windows.InvalidHandle
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("released diagnostic profile was not cleaned: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup did not observe the released file handle")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("owned diagnostic profile remains after successful cleanup: %v", err)
	}
}

func TestDiagnosticCleanupDeadlineRetainsWindowsError(t *testing.T) {
	dir, handle := lockDiagnosticProfileFile(t)
	defer windows.CloseHandle(handle)
	p := &diagnosticProcess{dir: dir}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := p.closeWithin(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !retryDiagnosticCleanup(err) {
		t.Fatalf("cleanup lost its deadline or final Windows file error: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("locked profile exceeded the existing cleanup deadline")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locked profile disappearance incorrectly implied successful cleanup: %v", err)
	}
	b := &Browser{diagnosticProcess: p}
	if again := b.CloseDiagnostic(); again != err {
		t.Fatalf("CloseDiagnostic discarded or replaced the original cleanup failure: %v", again)
	}
	if again := b.CloseDiagnostic(); again != err {
		t.Fatalf("repeated close lost its original cleanup failure: %v", again)
	}
}

func TestPoolShutdownReportsWindowsProfileCleanupFailure(t *testing.T) {
	for _, diagnostic := range []bool{false, true} {
		name := "ordinary"
		if diagnostic {
			name = "diagnostic"
		}
		t.Run(name, func(t *testing.T) {
			dir, handle := lockDiagnosticProfileFile(t)
			defer windows.CloseHandle(handle)
			b := testBrowser(true, true)
			if diagnostic {
				process := &diagnosticProcess{dir: dir}
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
				defer cancel()
				if err := process.closeWithin(ctx); err == nil {
					t.Fatal("locked profile unexpectedly deleted")
				}
				b.diagnosticProcess = process
			} else {
				b.launcher = &ownedLauncher{config: launcher.New().UserDataDir(dir), profileDir: dir}
			}
			p := newTestPool(1, 0, func(bool) (*Browser, error) { return b, nil })
			if _, err := p.Get(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := p.Shutdown(ctx)
			if err == nil || !retryDiagnosticCleanup(err) {
				t.Fatalf("shutdown hid owned profile deletion failure: %v", err)
			}
			if p.Stats().Live != 0 {
				t.Fatal("completed process cleanup should release physical capacity")
			}
			if err := p.Shutdown(ctx); err == nil {
				t.Fatal("repeated shutdown lost profile deletion failure")
			}
		})
	}
}
