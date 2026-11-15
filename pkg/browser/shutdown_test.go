package browser

import (
	"context"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/shirou/gopsutil/v4/process"
	"sync"
	"testing"
	"time"
)

func TestPoolCloseReclaimsRealBorrowedBrowserWithoutLeakless(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("system browser unavailable")
	}
	p, err := NewPool(PoolConfig{Size: 1, BinarySource: BrowserBinarySystem, Headless: true, NoSandbox: true, Leakless: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pid := int32(b.launcher.PID())
	if pid == 0 {
		t.Fatal("missing browser pid")
	}
	if _, err := b.Browser.Version(); err != nil {
		t.Fatal("browser is not usable before shutdown", err)
	}
	p.Close() // Deliberately do not Put: this models a worker exceeding StopTimeout.
	exists, err := process.PidExists(pid)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("owned browser PID %d survived shutdown without leakless", pid)
	}
	if err := p.Put(b); err == nil {
		t.Fatal("return after close must fail")
	}
}

func TestPoolCloseReclaimsBorrowedBrowsers(t *testing.T) {
	p := newTestPool(1, 1, nil)
	proxy, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	direct, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	for _, b := range []*Browser{proxy, direct} {
		if b.IsAlive() {
			t.Fatal("checked-out browser survived shutdown")
		}
		if err := p.Put(b); err == nil {
			t.Fatal("return after shutdown must fail")
		}
	}
	if _, err := p.Get(context.Background()); err == nil {
		t.Fatal("closed pool admitted work")
	}
}

func TestPoolCloseWaitsForAndReclaimsLateCreation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	b := testBrowser(true, true)
	p := newTestPool(1, 0, func(bool) (*Browser, error) { close(started); <-release; return b, nil })
	result := make(chan error, 1)
	go func() { _, err := p.Get(context.Background()); result <- err }()
	<-started
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	deadline := time.After(time.Second)
	for !p.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("shutdown did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-closed:
		t.Fatal("shutdown returned before creation settled")
	default:
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck")
	}
	if err := <-result; err == nil {
		t.Fatal("late browser was handed out")
	}
	if b.IsAlive() {
		t.Fatal("late browser survived shutdown")
	}
}

func TestBrowserConcurrentClose(t *testing.T) {
	b := testBrowser(true, true)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.Close() }()
	}
	wg.Wait()
	if b.IsAlive() {
		t.Fatal("closed browser reported alive")
	}
}
