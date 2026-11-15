package browser

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
)

func TestOwnedGuardCancellationDuringPIDWait(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var pid int
		_, e := acceptOwnedGuard(ctx, listener, make(chan struct{}), "fixture", &pid)
		done <- e
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Read the parent's UID message, then deliberately never send a PID.
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 128)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("guard PID wait ignored cancellation")
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("guard socket not closed")
	}
}

type stalledHealthCDP struct{ entered chan struct{} }

func (c *stalledHealthCDP) Event() <-chan *cdp.Event { return nil }
func (c *stalledHealthCDP) Call(ctx context.Context, _, _ string, _ interface{}) ([]byte, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBrowserHealthProbeRespectsCancellation(t *testing.T) {
	client := &stalledHealthCDP{entered: make(chan struct{})}
	b := &Browser{Browser: rod.New().Client(client)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- b.IsAliveContext(ctx) }()
	<-client.entered
	cancel()
	select {
	case alive := <-done:
		if alive {
			t.Fatal("cancelled probe reported healthy")
		}
	case <-time.After(time.Second):
		t.Fatal("health probe ignored cancellation")
	}
	if b.Browser.GetContext().Err() != nil {
		t.Fatal("probe changed browser lifetime")
	}
}
