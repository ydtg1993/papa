package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestShutdownCancelsOwnedStartupWithoutBorrowerCancellation(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range []bool{false, true} {
		t.Run(strconv.FormatBool(diagnostic), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv("PAPA_DIAGNOSTIC_STARTUP_HELPER", "launch")
			t.Setenv("PAPA_DIAGNOSTIC_STARTUP_PID", marker)
			cfg := PoolConfig{Size: 1, BrowserPath: bin, TempDir: t.TempDir(), Network: egress.Config{AllowedOrigins: []string{"http://127.0.0.1:1"}}}
			var p *Pool
			if diagnostic {
				p, err = NewDiagnosticPool(cfg)
			} else {
				p, err = NewPool(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			borrowCtx, cancelBorrow := context.WithCancel(context.Background())
			defer cancelBorrow()
			go func() { _, e := p.Get(borrowCtx); result <- e }()
			deadline := time.Now().Add(5 * time.Second)
			var data []byte
			for time.Now().Before(deadline) {
				data, err = os.ReadFile(marker)
				if err == nil && len(data) > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(data) == 0 {
				t.Fatal("helper did not start", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err = p.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-result; err == nil {
				t.Fatal("cancelled startup returned success")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if exists, e := process.PidExists(int32(pid)); e != nil || exists {
				t.Fatalf("startup helper survived: pid=%d exists=%v err=%v", pid, exists, e)
			}
		})
	}
}

func TestShutdownReclaimsBorrowedAfterClose(t *testing.T) {
	p := newTestPool(1, 1, nil)
	a, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if !a.IsAlive() || !b.IsAlive() {
		t.Fatal("Close changed borrowed-work contract")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if a.IsAlive() || b.IsAlive() || p.Stats().Live != 0 {
		t.Fatal("borrowed browsers survived terminal shutdown")
	}
	_ = p.Put(a)
	_ = p.Put(b)
	if p.Stats().Live != 0 || p.proxyAlive.Load() != 0 || p.directAlive.Load() != 0 {
		t.Fatal("late return double-released slots")
	}
}

func TestShutdownDeadlineAndLateFactoryCleanup(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	b := testBrowser(true, true)
	p := newTestPool(1, 0, func(bool) (*Browser, error) { close(started); <-release; return b, nil })
	result := make(chan error, 1)
	go func() { _, err := p.Get(context.Background()); result <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if p.Stats().Creating != 1 {
		t.Fatal("in-flight factory hidden")
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("late result handed out")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := p.Shutdown(ctx2); err != nil {
		t.Fatal(err)
	}
	if b.IsAlive() || p.Stats().Live != 0 || p.Stats().Creating != 0 {
		t.Fatal("late factory leaked")
	}
}

type shutdownBlockedLauncher struct{ done chan struct{} }

func (*shutdownBlockedLauncher) Kill()                 {}
func (l *shutdownBlockedLauncher) Cleanup()            { <-l.done }
func (*shutdownBlockedLauncher) Get(flags.Flag) string { return "" }

func TestShutdownWaitsForAlreadyClosingProcess(t *testing.T) {
	l := &shutdownBlockedLauncher{done: make(chan struct{})}
	b := testBrowser(true, true)
	b.launcher = l
	b.cleanupTimeout = time.Millisecond
	p := newTestPool(1, 0, func(bool) (*Browser, error) { return b, nil })
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unfinished cleanup reported complete", err)
	}
	close(l.done)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := p.Shutdown(ctx2); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownRetainsTimedOutDiagnosticProcess(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "late-profile")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	diagnostic := &diagnosticProcess{done: make(chan struct{}), dir: dir, err: context.DeadlineExceeded}
	// Model a completed close attempt whose process wait exceeded its budget.
	diagnostic.once.Do(func() {})
	b := testBrowser(true, true)
	b.diagnosticProcess = diagnostic
	p := newTestPool(1, 0, func(bool) (*Browser, error) { return b, nil })
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("diagnostic falsely reported drained", err)
	}
	if p.Stats().Closing != 1 {
		t.Fatal("unfinished diagnostic missing from stats")
	}
	close(diagnostic.done)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := p.Shutdown(ctx2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("late process exit did not finish owned profile cleanup: %v", err)
	}
}

func TestShutdownAcceptsDiagnosticExitJustAfterCleanupDeadline(t *testing.T) {
	done := make(chan struct{})
	close(done)
	process := &diagnosticProcess{done: done, err: context.DeadlineExceeded}
	process.once.Do(func() {})
	b := testBrowser(true, true)
	b.diagnosticProcess = process
	p := newTestPool(1, 0, func(bool) (*Browser, error) { return b, nil })
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("completed diagnostic retained obsolete process timeout: %v", err)
	}
}

func TestSelfCheckReleasesRegisteredProcess(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("system browser unavailable")
	}
	root := t.TempDir()
	p, err := NewPool(PoolConfig{Size: 1, SelfCheck: true, TempDir: root, BinarySource: BrowserBinarySystem, Headless: true, NoSandbox: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if stats := p.Stats(); stats.Closed || stats.Live != 0 || stats.Creating != 0 {
		t.Fatalf("self-check changed admission or retained its temporary slot: %+v", stats)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("successful self-check left temporary profiles: %v %v", entries, err)
	}
}

func TestShutdownRealBorrowedBrowserWithoutLeakless(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("system browser unavailable")
	}
	p, err := NewPool(PoolConfig{Size: 1, BinarySource: BrowserBinarySystem, Headless: true, NoSandbox: true, Leakless: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Browser.Version(); err != nil {
		t.Fatal(err)
	}
	owned, ok := b.launcher.(*ownedLauncher)
	if !ok {
		t.Fatal("browser not owned")
	}
	pid := int32(owned.cmd.Process.Pid)
	root, err := process.NewProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	children, _ := root.Children()
	if err = p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, proc := range append(children, root) {
		exists, err := process.PidExists(proc.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("owned browser process %d survived shutdown", proc.Pid)
		}
	}
}
