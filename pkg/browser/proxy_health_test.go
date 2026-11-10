package browser

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

type fixtureProxyError struct{}

func (fixtureProxyError) Error() string        { return "fixture proxy transport failure" }
func (fixtureProxyError) ProxyFailure() string { return "transport" }

func TestFixedBrowserProxyFeedbackAttributionAndBorrowGeneration(t *testing.T) {
	p := newTestPool(1, 0, nil)
	defer p.Close()
	p.cfg.Flags = map[string]string{"proxy-server": "http://user:secret@proxy.invalid:1234"}
	address, observe, state, err := p.selectBrowserProxy(true, p.cfg.Flags["proxy-server"])
	if err != nil {
		t.Fatal(err)
	}
	if address != p.cfg.Flags["proxy-server"] || observe == nil {
		t.Fatal("fixed proxy changed")
	}
	b := &Browser{proxyState: state}
	for _, status := range []int{403, 429, 502} {
		report, _ := observe(context.Background())
		report(status, nil)
		if b.ProxyFailure() != nil {
			t.Fatalf("target %d polluted proxy health", status)
		}
	}
	report, _ := observe(context.Background())
	report(0, context.DeadlineExceeded)
	if b.ProxyFailure() != nil {
		t.Fatal("ordinary cancellation polluted proxy state")
	}
	old, _ := observe(context.Background())
	state.reset()
	old(0, fixtureProxyError{})
	if b.ProxyFailure() != nil {
		t.Fatal("previous borrow feedback polluted new borrow")
	}
	report, _ = observe(context.Background())
	report(0, fixtureProxyError{})
	if !errors.Is(b.ProxyFailure(), ErrFixedProxyUnavailable) || b.ProxyFailure().Error() != "fixed browser proxy unavailable" {
		t.Fatalf("confirmed fixed exit failure omitted or exposed URL: %v", b.ProxyFailure())
	}
	report, _ = observe(context.Background())
	report(http.StatusOK, nil)
	if b.ProxyFailure() == nil {
		t.Fatal("concurrent successful request erased this attempt's confirmed failure")
	}
	state.reset()
	if b.ProxyFailure() != nil {
		t.Fatal("new borrow retained old failure")
	}
	if !p.FixedProxyFailure(proxy.ErrUnavailable) || p.FixedProxyFailure(context.DeadlineExceeded) {
		t.Fatal("pre-borrow fixed proxy classification incorrect")
	}
}

func TestFixedBrowserProxyUnavailableNeverFallsBackToDirect(t *testing.T) {
	m := proxy.NewManager("", 0)
	defer m.Close()
	cfg := proxy.DefaultHealthConfig()
	cfg.Enabled = true
	cfg.FailureThreshold = 1
	if err := m.ConfigureHealth(cfg); err != nil {
		t.Fatal(err)
	}
	address := "http://proxy.invalid:1234"
	ref, err := m.AcquireURL(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	ref.Report(proxy.Feedback{Kind: proxy.TransportFailure})
	p := newTestPool(1, 0, nil)
	defer p.Close()
	p.cfg.ProxyManager = m
	p.cfg.Flags = map[string]string{"proxy-server": address}
	upstream, observer, _, err := p.selectBrowserProxy(true, address)
	if !errors.Is(err, proxy.ErrUnavailable) || upstream != "" || observer != nil {
		t.Fatalf("unavailable fixed proxy silently changed exit: %q %v", upstream, err)
	}
}
