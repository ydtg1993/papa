package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func healthyManager(t *testing.T, addresses ...string) (*Manager, *time.Time) {
	t.Helper()
	now := time.Now()
	m := NewManager("", 0)
	m.apiURL = "http://provider.invalid"
	m.now = func() time.Time { return now }
	c := DefaultHealthConfig()
	c.Enabled, c.FailureThreshold, c.RecoverySuccesses = true, 1, 2
	if err := m.ConfigureHealth(c); err != nil {
		t.Fatal(err)
	}
	m.replaceProxies(addresses)
	t.Cleanup(m.Close)
	return m, &now
}

func acquire(t *testing.T, m *Manager) *Reference {
	t.Helper()
	r, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHealthDefaultAndConfiguredExhaustion(t *testing.T) {
	m := NewManager("", 0)
	t.Cleanup(m.Close)
	if m.HealthConfig().Enabled {
		t.Fatal("health enabled by default")
	}
	if r, err := m.Acquire(context.Background()); err != nil || r.URL() != "" {
		t.Fatalf("unconfigured direct access: %v %v", r, err)
	}
	m.apiURL = "http://provider.invalid"
	if r, err := m.Acquire(context.Background()); err != nil || r.URL() != "" {
		t.Fatalf("disabled compatibility: %v %v", r, err)
	}
	c := DefaultHealthConfig()
	c.Enabled = true
	if err := m.ConfigureHealth(c); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("configured exhaustion must fail closed: %v", err)
	}
	c.FailureThreshold = 0
	if err := m.ConfigureHealth(c); err == nil || m.HealthConfig().FailureThreshold == 0 {
		t.Fatal("invalid config published")
	}
}

func TestHealthCooldownProbeRecoveryAndOldResponses(t *testing.T) {
	m, now := healthyManager(t, "http://127.0.0.1:8080")
	old := acquire(t, m)
	r := acquire(t, m)
	r.Report(Feedback{Kind: TransportFailure})
	r.Report(Feedback{Kind: Success})
	old.Report(Feedback{Kind: Success})
	if s := m.Snapshot()[0]; s.State != StateCooling || s.InFlight != 0 {
		t.Fatalf("late success reopened cooldown or double release: %+v", s)
	}
	if _, err := m.Acquire(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cooling selected: %v", err)
	}
	*now = now.Add(m.health.InitialCooldown)
	probe := acquire(t, m)
	if _, err := m.Acquire(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("probe limit exceeded: %v", err)
	}
	probe.ReportHTTP(http.StatusOK, nil)
	if s := m.Snapshot()[0]; s.State != StateProbing || s.RecoverySuccesses != 1 {
		t.Fatalf("premature recovery: %+v", s)
	}
	acquire(t, m).ReportHTTP(http.StatusOK, nil)
	if s := m.Snapshot()[0]; s.State != StateAvailable || s.Probes != 0 {
		t.Fatalf("probe recovery did not finish: %+v", s)
	}
}

func TestHealthProbeBackoffAndGlobalBudget(t *testing.T) {
	m, now := healthyManager(t, "http://127.0.0.1:8080", "http://127.0.0.1:8081")
	for range 2 {
		acquire(t, m).Report(Feedback{Kind: TransportFailure})
	}
	*now = now.Add(m.health.InitialCooldown)
	probe := acquire(t, m)
	if _, err := m.Acquire(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("global probe limit exceeded: %v", err)
	}
	probe.Report(Feedback{Kind: TransportFailure})
	for _, s := range m.Snapshot() {
		if s.ID == probe.ID() && s.CooldownRemaining != 2*m.health.InitialCooldown {
			t.Fatalf("failed probe did not back off: %+v", s)
		}
	}
	other := acquire(t, m)
	if other.ID() == probe.ID() {
		t.Fatal("failed probe immediately reused")
	}
	other.Release()
}

func TestHealthProviderRefreshRetainsStateAndPrunesAfterDrain(t *testing.T) {
	const address = "http://127.0.0.1:8080"
	m, _ := healthyManager(t, address)
	old := acquire(t, m)
	acquire(t, m).Report(Feedback{Kind: TransportFailure})
	m.replaceProxies([]string{address})
	if m.Snapshot()[0].State != StateCooling {
		t.Fatal("refresh forgot health")
	}
	m.replaceProxies([]string{"http://127.0.0.1:8081"})
	if len(m.Snapshot()) != 2 {
		t.Fatal("removed in-flight record was lost")
	}
	m.replaceProxies([]string{address})
	if s := m.Snapshot(); len(s) != 1 || s[0].State != StateCooling {
		t.Fatalf("re-added proxy forgot in-flight health: %+v", s)
	}
	m.replaceProxies(nil)
	old.Release()
	if len(m.Snapshot()) != 0 {
		t.Fatal("retired record retained after drain")
	}
	for i := range 500 {
		m.replaceProxies([]string{fmt.Sprintf("http://127.0.0.1:%d", 10000+i)})
	}
	if len(m.Snapshot()) != 1 {
		t.Fatal("inactive provider history grew unbounded")
	}
}

func TestHealthFixedProxyAndAdministrativeProbe(t *testing.T) {
	m, _ := healthyManager(t, "http://127.0.0.1:8080")
	fixed := "http://private-user:private-password@127.0.0.1:8081"
	r, err := m.AcquireURL(context.Background(), fixed)
	if err != nil || r.URL() != fixed {
		t.Fatalf("fixed proxy changed: %v %v", r, err)
	}
	r.ReportHTTP(http.StatusProxyAuthRequired, nil)
	if _, err := m.AcquireURL(context.Background(), fixed); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("fixed proxy rotated or bypassed: %v", err)
	}
	if err := m.Disable(r.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AcquireProbe(context.Background(), r.ID()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("disabled proxy probed: %v", err)
	}
	if err := m.Enable(r.ID()); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Snapshot() {
		if s.ID == r.ID() && s.State != StateProbing {
			t.Fatal("enable fabricated a successful probe")
		}
	}
	probe, err := m.AcquireProbe(context.Background(), r.ID())
	if err != nil {
		t.Fatal(err)
	}
	probe.Release()
	b, _ := json.Marshal(m.Snapshot())
	for _, secret := range []string{"private-user", "private-password", "127.0.0.1", "http://"} {
		if strings.Contains(string(b), secret) || strings.Contains(fmt.Sprint(r), secret) {
			t.Fatalf("snapshot or reference leaks %q", secret)
		}
	}
}

func TestHealthClassifiesOnlyProxyFailures(t *testing.T) {
	proxyURL := "http://127.0.0.1:8080"
	cases := []struct {
		name   string
		status int
		err    error
		want   FailureKind
	}{
		{"success", 200, nil, Success},
		{"account forbidden", 403, nil, Neutral},
		{"site rate limit", 429, nil, Neutral},
		{"site unavailable", 503, nil, Neutral},
		{"proxy auth", 407, nil, AuthenticationFailure},
		{"cancel", 0, context.Canceled, Neutral},
		{"deadline", 0, context.DeadlineExceeded, Neutral},
		{"unidentified failure", 0, errors.New("connection failed"), Neutral},
		{"proxy dial", 0, &net.OpError{Op: "dial", Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, Err: errors.New("refused")}, TransportFailure},
		{"target dial", 0, &net.OpError{Op: "dial", Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8081}, Err: errors.New("refused")}, Neutral},
		{"target read", 0, &net.OpError{Op: "read", Addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, Err: errors.New("timeout")}, Neutral},
		{"proxy connect", 0, &net.OpError{Op: "proxyconnect", Err: errors.New("refused")}, TransportFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyHTTP(proxyURL, tc.status, tc.err); got != tc.want {
				t.Fatalf("classification %s, want %s", got, tc.want)
			}
		})
	}
	m, _ := healthyManager(t, proxyURL)
	for _, kind := range []FailureKind{SiteBlocked, Neutral, "account_expired", "unknown"} {
		acquire(t, m).Report(Feedback{Kind: kind})
	}
	if m.Snapshot()[0].State != StateAvailable {
		t.Fatal("site/account failures poisoned proxy")
	}
}

func TestHealthWaitingCancellationRefreshAndClose(t *testing.T) {
	m, _ := healthyManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.WaitAcquire(ctx); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait ignored cancellation")
	}
	go func() {
		r, err := m.WaitAcquire(context.Background())
		if r != nil {
			r.Release()
		}
		done <- err
	}()
	m.replaceProxies([]string{"http://127.0.0.1:8080"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh did not wake waiter")
	}
	m.replaceProxies(nil)
	go func() { _, err := m.WaitAcquire(context.Background()); done <- err }()
	m.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not release waiter")
	}
}

func TestHealthConcurrentAcquireRefreshAndAdministration(t *testing.T) {
	m, _ := healthyManager(t, "http://127.0.0.1:8080", "http://127.0.0.1:8081")
	var wg sync.WaitGroup
	for worker := range 12 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for range 100 {
				if worker%4 == 0 {
					m.replaceProxies([]string{"http://127.0.0.1:8080", "http://127.0.0.1:8081"})
				}
				if worker%4 == 1 {
					for _, s := range m.Snapshot() {
						_ = m.Disable(s.ID)
						_ = m.Enable(s.ID)
					}
				}
				r, err := m.Acquire(context.Background())
				if err != nil {
					if !errors.Is(err, ErrUnavailable) {
						t.Error(err)
					}
					continue
				}
				var reports sync.WaitGroup
				reports.Add(2)
				go func() { defer reports.Done(); r.ReportHTTP(200, nil) }()
				go func() { defer reports.Done(); r.Release() }()
				reports.Wait()
			}
		}(worker)
	}
	wg.Wait()
	for _, s := range m.Snapshot() {
		if s.InFlight != 0 || s.Probes != 0 {
			t.Fatalf("concurrent completion leaked counts: %+v", s)
		}
	}
}

func TestHealthWaitsForActualCooldownExpiry(t *testing.T) {
	m, _ := healthyManager(t, "http://127.0.0.1:8080")
	m.now = nil
	c := m.HealthConfig()
	c.InitialCooldown, c.MaxCooldown = 10*time.Millisecond, 20*time.Millisecond
	if err := m.ConfigureHealth(c); err != nil {
		t.Fatal(err)
	}
	acquire(t, m).Report(Feedback{Kind: TransportFailure})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r, err := m.WaitAcquire(ctx)
	if err != nil {
		t.Fatalf("cooldown expiry did not wake waiter: %v", err)
	}
	r.Release()
}

func TestHealthAdministrativeCASAndProbeReservation(t *testing.T) {
	m, _ := healthyManager(t, "http://127.0.0.1:8080")
	initial := m.Snapshot()[0]
	acquire(t, m).ReportHTTP(200, nil)
	if err := m.DisableCAS(initial.ID, initial.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale feedback version accepted: %v", err)
	}
	current := m.Snapshot()[0]
	if err := m.DisableCAS(current.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	if err := m.EnableCAS(current.ID, current.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale enable accepted: %v", err)
	}
	current = m.Snapshot()[0]
	if err := m.EnableCAS(current.ID, current.Version); err != nil {
		t.Fatal(err)
	}
	current = m.Snapshot()[0]
	probe, err := m.AcquireProbeCAS(context.Background(), current.ID, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Release()
	if _, err := m.AcquireProbeCAS(context.Background(), current.ID, current.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate probe action accepted: %v", err)
	}
}

func TestFixedHealthCapacityCannotEvictUnavailableOrInFlightIdentity(t *testing.T) {
	m, _ := healthyManager(t)
	for i := range MaxFixedHealthEntries {
		address := fmt.Sprintf("http://127.0.0.1:%d", i+10000)
		e := newEntry(address)
		e.fixed, e.state = true, StateDisabled
		m.entries[address] = e
	}
	const next = "http://127.0.0.1:9999"
	if _, err := m.AcquireURL(context.Background(), next); !errors.Is(err, ErrHealthCapacity) {
		t.Fatalf("unavailable history was forgotten: %v", err)
	}
	for _, e := range m.entries {
		e.state = StateAvailable
		break
	}
	r, err := m.AcquireURL(context.Background(), next)
	if err != nil {
		t.Fatal(err)
	}
	r.Release()
	if len(m.Snapshot()) != MaxFixedHealthEntries {
		t.Fatal("fixed state capacity was exceeded")
	}
}
