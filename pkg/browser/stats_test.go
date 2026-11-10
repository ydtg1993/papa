package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/governance"
)

func TestPoolStatsLiveBorrowIdleCloseWithoutResourceControl(t *testing.T) {
	p := newTestPool(2, 1, nil)
	defer p.Close()
	if s := p.Stats(); s.Capacity != 3 || s.Live != 0 || s.Borrowed != 0 {
		t.Fatalf("empty stats=%+v", s)
	}
	proxy, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	direct, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s := p.Stats(); s.Live != 2 || s.Borrowed != 2 || s.Proxy.Live != 1 || s.Direct.Live != 1 {
		t.Fatalf("borrowed stats=%+v", s)
	}
	p.Put(proxy)
	if s := p.Stats(); s.Live != 2 || s.Borrowed != 1 || s.Idle != 1 {
		t.Fatalf("returned stats=%+v", s)
	}
	p.Close()
	if s := p.Stats(); !s.Closed || s.Live != 1 || s.Borrowed != 1 || s.Idle != 0 {
		t.Fatalf("pool close must drain idle, retain actual borrowed instance: %+v", s)
	}
	p.Put(direct)
	direct.Close()
	if s := p.Stats(); s.Live != 0 || s.Borrowed != 0 {
		t.Fatalf("close/put leaked or double-decremented: %+v", s)
	}
}

func TestPoolStatsResourceReservationIsNotAStartedOrBorrowedBrowser(t *testing.T) {
	ctx, c := governanceContext(t)
	held, _ := c.Acquire(ctx, "other", governance.ClassBrowser)
	p := newTestPool(1, 0, nil)
	defer p.Close()
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		b, err := p.Get(waitCtx)
		if b != nil {
			p.Put(b)
		}
		done <- err
	}()
	awaitGovernance(t, func() bool { return c.Snapshot().Browser.Waiting == 1 })
	if s := p.Stats(); s.Live != 0 || s.Borrowed != 0 || s.Capacity != 1 {
		t.Fatalf("pre-creation reservation counted live: %+v", s)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	held()
	b, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Put(b)
	held, _ = c.Acquire(ctx, "other", governance.ClassBrowser)
	defer held()
	waitCtx, cancel = context.WithCancel(ctx)
	defer cancel()
	go func() {
		b, err := p.Get(waitCtx)
		if b != nil {
			p.Put(b)
		}
		done <- err
	}()
	awaitGovernance(t, func() bool { return c.Snapshot().Browser.Waiting == 1 })
	if s := p.Stats(); s.Live != 1 || s.Borrowed != 0 || s.Reserved != 1 || s.Idle != 0 {
		t.Fatalf("idle instance pending resource counted borrowed: %+v", s)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resource wait did not cancel")
	}
	if s := p.Stats(); s.Live != 1 || s.Idle != 1 || s.Borrowed != 0 || s.Reserved != 0 {
		t.Fatalf("cancelled reservation not returned: %+v", s)
	}
}

func TestPoolStatsFactoryFailureAndExplicitInstanceClose(t *testing.T) {
	p := newTestPool(1, 0, func(bool) (*Browser, error) { return nil, errors.New("fixture") })
	defer p.Close()
	if _, err := p.Get(context.Background()); err == nil {
		t.Fatal("missing creation failure")
	}
	if p.Stats().Live != 0 {
		t.Fatal("failed creation counted live")
	}
	p.newBrowserFn = func(useProxy bool) (*Browser, error) { return testBrowser(useProxy, true), nil }
	b, err := p.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	b.Close()
	p.Put(b)
	if s := p.Stats(); s.Live != 0 || s.Borrowed != 0 {
		t.Fatalf("explicit Close did not unregister: %+v", s)
	}
	if next, err := p.Get(context.Background()); err != nil {
		t.Fatalf("closed instance did not free capacity: %v", err)
	} else {
		p.Put(next)
	}
}

func TestPoolStatsCloseDuringCreationDoesNotPublishOrLeakBrowser(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	p := newTestPool(1, 0, func(useProxy bool) (*Browser, error) {
		close(started)
		<-finish
		return testBrowser(useProxy, true), nil
	})
	defer p.Close()
	done := make(chan error, 1)
	go func() { _, err := p.Get(context.Background()); done <- err }()
	<-started
	if p.Stats().Live != 0 {
		t.Fatal("incomplete browser creation counted live")
	}
	p.Close()
	close(finish)
	if err := <-done; err == nil {
		t.Fatal("closed pool published newly created browser")
	}
	if s := p.Stats(); s.Live != 0 || s.Borrowed != 0 || p.proxyAlive.Load() != 0 {
		t.Fatalf("creation completed after pool Close leaked: %+v", s)
	}
}
