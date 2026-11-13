package browser

import (
	"context"
	"errors"
	"sync"

	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
)

var ErrFixedProxyUnavailable = errors.New("fixed browser proxy unavailable")

type browserProxyState struct {
	mu         sync.Mutex
	fixed      bool
	generation uint64
	failure    error
}

func (s *browserProxyState) reset()          { s.mu.Lock(); s.generation++; s.failure = nil; s.mu.Unlock() }
func (s *browserProxyState) version() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.generation }
func (s *browserProxyState) report(version uint64, kind proxy.FailureKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fixed || version != s.generation {
		return
	}
	switch kind {
	case proxy.TransportFailure, proxy.AuthenticationFailure:
		s.failure = ErrFixedProxyUnavailable
	}
}

// ProxyFailure reports only a confirmed fixed-exit failure for this borrow.
// It never exposes the proxy URL or treats ordinary target errors as failures.
func (b *Browser) ProxyFailure() error {
	if b == nil || b.proxyState == nil {
		return nil
	}
	s := b.proxyState
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// FixedProxyFailure identifies pre-borrow fixed-proxy admission failures.
func (p *Pool) FixedProxyFailure(err error) bool {
	if p == nil || err == nil || p.newLauncher().Get(flags.Flag("proxy-server")) == "" {
		return false
	}
	return errors.Is(err, proxy.ErrUnavailable) || errors.Is(err, ErrFixedProxyUnavailable)
}

// The exit is frozen for the lifetime of the instance. Feedback references are
// acquired for that same address, never used to rotate a live browser's exit.
func (p *Pool) selectBrowserProxy(useProxy bool, fixed string) (string, func(context.Context) (func(int, error), error), *browserProxyState, error) {
	m := p.cfg.ProxyManager
	if fixed == "" && (!useProxy || m == nil) {
		return "", nil, nil, nil
	}
	state := &browserProxyState{fixed: fixed != ""}
	upstream := fixed
	if m != nil {
		p.mu.Lock()
		ctx := p.admissionCtx
		p.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		ref, err := m.AcquireURL(ctx, fixed)
		if err != nil {
			return "", nil, nil, err
		}
		upstream = ref.URL()
		ref.Release()
	}
	if upstream == "" {
		return "", nil, nil, nil
	}
	observe := func(ctx context.Context) (func(int, error), error) {
		version := state.version()
		if m == nil {
			return func(status int, err error) { state.report(version, proxy.ClassifyHTTP(upstream, status, err)) }, nil
		}
		ref, err := m.AcquireURL(ctx, upstream)
		if err != nil {
			if errors.Is(err, proxy.ErrUnavailable) {
				state.report(version, proxy.TransportFailure)
			}
			return nil, err
		}
		return func(status int, err error) {
			state.report(version, proxy.ClassifyHTTP(upstream, status, err))
			ref.ReportHTTP(status, err)
		}, nil
	}
	return upstream, observe, state, nil
}
