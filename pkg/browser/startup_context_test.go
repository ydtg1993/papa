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

	"github.com/shirou/gopsutil/v4/process"
)

func TestPoolProductionStartupCancellation(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(strconv.FormatBool(guarded), func(t *testing.T) { productionStartupCancellation(t, guarded) })
	}
}

func productionStartupCancellation(t *testing.T, guarded bool) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"borrow", "selfcheck", "close", "handshake"} {
		t.Run(mode, func(t *testing.T) {
			root, marker := t.TempDir(), filepath.Join(t.TempDir(), "child.pid")
			helper := "launch"
			if mode == "handshake" {
				helper = "handshake"
			}
			t.Setenv("PAPA_DIAGNOSTIC_STARTUP_HELPER", helper)
			t.Setenv("PAPA_DIAGNOSTIC_STARTUP_PID", marker)
			cfg := PoolConfig{Size: 1, BrowserPath: bin, TempDir: root, Headless: true, Leakless: guarded, SelfCheck: mode == "selfcheck"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var pool *Pool
			if mode != "selfcheck" {
				pool, err = NewPool(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
			}
			done := make(chan error, 1)
			go func() {
				if mode == "selfcheck" {
					p, e := NewPoolContext(ctx, cfg)
					if p != nil {
						p.Close()
					}
					done <- e
				} else {
					_, e := pool.Get(ctx)
					done <- e
				}
			}()
			waitPool(t, func() bool { _, e := os.Stat(marker); return e == nil })
			if mode == "handshake" {
				waitPool(t, func() bool { _, e := os.Stat(marker + ".connected"); return e == nil })
			}
			if mode == "close" {
				pool.Close()
			} else {
				cancel()
			}
			select {
			case e := <-done:
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("cancellation lost: %v", e)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("startup did not cancel")
			}
			data, e := os.ReadFile(marker)
			if e != nil {
				t.Fatal(e)
			}
			pid, e := strconv.Atoi(strings.TrimSpace(string(data)))
			if e != nil {
				t.Fatal(e)
			}
			waitPool(t, func() bool { alive, _ := process.PidExists(int32(pid)); return !alive })
			entries, e := os.ReadDir(root)
			if e != nil || len(entries) != 0 {
				t.Fatalf("profile leaked: %v %v", entries, e)
			}
			if pool != nil && (pool.Stats().Creating != 0 || pool.proxyAlive.Load() != 0) {
				t.Fatalf("reservation leaked: %+v", pool.Stats())
			}
		})
	}
}

func TestPoolProductionBorrowContextDoesNotOwnBrowser(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(strconv.FormatBool(guarded), func(t *testing.T) { productionBorrowContext(t, guarded) })
	}
}

func productionBorrowContext(t *testing.T, guarded bool) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	p, err := NewPool(PoolConfig{Size: 1, BrowserPath: bin, Headless: true, Leakless: guarded, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	b, err := p.Get(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if err := p.Put(b); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	next, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Put(next)
	if next != b || !next.IsAlive() {
		t.Fatal("cancelled borrower destroyed reusable browser")
	}
}

func TestPoolProductionChromiumStartupDeadline(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(strconv.FormatBool(guarded), func(t *testing.T) { productionChromiumDeadline(t, guarded) })
	}
}

func productionChromiumDeadline(t *testing.T, guarded bool) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	root := t.TempDir()
	p, err := NewPool(PoolConfig{Size: 1, BrowserPath: bin, Headless: true, Leakless: guarded, TempDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	b, err := p.Get(ctx)
	if b != nil {
		b.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup deadline lost: %v", err)
	}
	if s := p.Stats(); s.Live != 0 || s.Creating != 0 || p.proxyAlive.Load() != 0 {
		t.Fatalf("cancelled Chromium retained admission: %+v", s)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled Chromium profile leaked: %v %v", entries, err)
	}
}
