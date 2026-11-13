package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/shirou/gopsutil/v4/process"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

// A real child executable deliberately fails to announce Chrome's endpoint or
// complete the WebSocket handshake. The production diagnostic startup path must
// terminate it; returning while leaving a blocked startup goroutine is a failure.
func init() {
	mode := os.Getenv("PAPA_DIAGNOSTIC_STARTUP_HELPER")
	if mode == "" {
		return
	}
	_ = os.WriteFile(os.Getenv("PAPA_DIAGNOSTIC_STARTUP_PID"), []byte(strconv.Itoa(os.Getpid())), 0600)
	if mode == "handshake" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "DevTools listening on ws://%s/devtools/browser/fixture\n", listener.Addr())
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(3)
		}
		defer conn.Close()
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestIdentityDiagnosticStartupCancellationKillsOwnedProcess(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"launch", "handshake"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv("PAPA_DIAGNOSTIC_STARTUP_HELPER", mode)
			t.Setenv("PAPA_DIAGNOSTIC_STARTUP_PID", marker)
			pool, err := NewDiagnosticPool(PoolConfig{Size: 1, Headless: true, BrowserPath: bin, TempDir: root, Network: egress.Config{AllowedOrigins: []string{"http://127.0.0.1:1"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err = pool.Get(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline lost: %v", err)
			}
			if time.Since(started) > 8*time.Second {
				t.Fatal("diagnostic startup exceeded cleanup bound")
			}
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal("helper did not run", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatal(err)
			}
			if alive, err := process.PidExists(int32(pid)); err != nil || alive {
				t.Fatalf("owned child still alive: pid=%d alive=%v err=%v", pid, alive, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("diagnostic profile not cleaned: %v %v", entries, err)
			}
			if pool.proxyAlive.Load() != 0 {
				t.Fatal("cancelled startup leaked pool slot")
			}
		})
	}
}

func TestIdentityDiagnosticPoolRejectsUnsupportedGuard(t *testing.T) {
	if _, err := NewDiagnosticPool(PoolConfig{Size: 1, Leakless: true}); err == nil {
		t.Fatal("leakless silently ignored")
	}
}

func TestCloseDiagnosticReturnsPoolSlotExactlyOnce(t *testing.T) {
	p, err := NewDiagnosticPool(PoolConfig{Size: 1, TempDir: t.TempDir(), Network: egress.Config{AllowedOrigins: []string{"http://127.0.0.1:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	alive := true
	p.newBrowserFn = func(useProxy bool) (*Browser, error) {
		dir, err := os.MkdirTemp(p.TempDir(), "browser-diagnostic-")
		return &Browser{diagnosticProcess: &diagnosticProcess{dir: dir}, useProxy: useProxy, aliveOverride: &alive}, err
	}
	for range 2 {
		if err := p.WithBrowser(context.Background(), func(b *Browser) error {
			if err := b.CloseDiagnostic(); err != nil {
				return err
			}
			return b.CloseDiagnostic()
		}); err != nil {
			t.Fatal(err)
		}
		if p.proxyAlive.Load() != 0 || p.directAlive.Load() != 0 || len(p.browsers) != 0 || len(p.direct) != 0 {
			t.Fatalf("closed diagnostic did not return exactly one slot: proxy=%d direct=%d idle=%d", p.proxyAlive.Load(), p.directAlive.Load(), len(p.browsers)+len(p.direct))
		}
	}
}

func TestCloseDiagnosticRejectsOrdinaryBrowserWithoutClosingIt(t *testing.T) {
	b := &Browser{}
	if err := b.CloseDiagnostic(); err == nil || b.closed.Load() {
		t.Fatalf("diagnostic close changed an ordinary browser: %v", err)
	}
}

func TestIdentityDiagnosticStartupRealChromeUsesManagedFactory(t *testing.T) {
	bin := os.Getenv("PAPA_TEST_CHROME")
	if bin == "" {
		t.Skip("PAPA_TEST_CHROME not configured")
	}
	p, err := NewDiagnosticPool(PoolConfig{Size: 1, Headless: true, BrowserPath: bin, TempDir: t.TempDir(), Network: egress.Config{AllowedOrigins: []string{"http://127.0.0.1:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = p.WithBrowser(ctx, func(b *Browser) error {
		r, err := b.ResolveIdentity(ctx, identityTestProfile("diagnostic"))
		if err != nil {
			return err
		}
		return b.WithPageOptions(ctx, PageOptions{Identity: r}, func(page *rod.Page) error {
			if err := page.Navigate("about:blank"); err != nil {
				return err
			}
			if err := page.WaitLoad(); err != nil {
				return err
			}
			o, err := ObserveIdentity(page)
			if err != nil {
				return err
			}
			if !o.Adapter.Verified {
				return fmt.Errorf("adapter did not execute")
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
