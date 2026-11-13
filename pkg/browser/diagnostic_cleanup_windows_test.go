//go:build windows

package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func lockDiagnosticProfileFile(t *testing.T) (string, windows.Handle) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "browser-diagnostic-owned")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
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
	return dir, handle
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
