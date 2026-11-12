package browserdiagnostics

import (
	"context"
	"errors"
	"github.com/go-rod/rod"
	"sync"

	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

type Service struct {
	once       sync.Once
	options    Options
	diagnostic sync.Mutex
	manager    *identityruntime.Manager
	mu         sync.Mutex
	reports    []IdentityDiagnosticSummary
	closing    bool
	pending    int
	changed    chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
}

func (a *Service) resolveBrowserIdentity(ctx context.Context, s identityruntime.Subject, p identity.Profile) (resolved *identity.ResolvedProfile, err error) {
	if a.options.Resources == nil {
		return nil, errors.New("未配置浏览器资源")
	}
	if s.Kind == "account" {
		if m := a.options.Resources.GetAccountManager(); m != nil {
			return m.ResolveIdentity(ctx, s, p)
		}
		return nil, errors.New("未配置账号管理器")
	}
	pool := a.options.Resources.GetBrowserPool()
	if pool == nil {
		return nil, errors.New("未配置公共浏览器池")
	}
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error { var e error; resolved, e = b.ResolveIdentity(ctx, p); return e })
	return
}

func (a *Service) verifyBrowserIdentity(ctx context.Context, s identityruntime.Subject, r *identity.ResolvedProfile) (func() error, error) {
	if a.options.Resources == nil {
		return nil, errors.New("未配置浏览器资源")
	}
	if s.Kind == "account" {
		if m := a.options.Resources.GetAccountManager(); m != nil {
			return m.VerifyIdentity(ctx, s, r)
		}
		return nil, errors.New("未配置账号管理器")
	}
	pool := a.options.Resources.GetBrowserPool()
	if pool == nil {
		return nil, errors.New("未配置公共浏览器池")
	}
	// Resolve already checked native capabilities. Applying the exact snapshot to
	// a managed blank page verifies initialization without visiting a target site.
	err := pool.WithBrowser(ctx, func(b *browser.Browser) error {
		return b.WithPageOptions(ctx, browser.PageOptions{Identity: r}, func(page *rod.Page) error {
			if err := page.Navigate("about:blank"); err != nil {
				return err
			}
			if err := page.WaitLoad(); err != nil {
				return err
			}
			o, err := browser.ObserveIdentity(page)
			if err != nil {
				return err
			}
			if r.Profile.Adapter.Enabled && !o.Adapter.Verified {
				return errors.New("身份适配器初始化未验证")
			}
			return nil
		})
	})
	return nil, err
}

// View owns its storage operation lifetime even when called outside the identity
// HTTP handler, for example by the account-pool view.
func (a *Service) View(parent context.Context, site string) (map[string]any, error) {
	ctx, done, ok := a.Begin(parent)
	if !ok {
		return nil, ErrClosing
	}
	defer done()
	m := a.Manager()
	if m == nil {
		return nil, errors.New("浏览器身份管理尚未初始化")
	}
	profiles, err := m.Store().ListProfiles(ctx, site)
	if err != nil {
		return nil, err
	}
	bindings, err := m.Snapshots(ctx, site)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	reports := make([]IdentityDiagnosticSummary, 0, len(a.reports))
	for _, report := range a.reports {
		if site == "" || report.Reference.Site == site {
			reports = append(reports, report)
		}
	}
	a.mu.Unlock()
	return map[string]any{"profiles": profiles, "bindings": bindings, "reports": reports}, nil
}

func (a *Service) Begin(parent context.Context) (context.Context, func(), bool) {
	s := a
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, false
	}
	if s.ctx == nil {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	s.pending++
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	life := s.ctx
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := func() bool { return false }
	if life != nil {
		stop = context.AfterFunc(life, cancel)
	}
	return ctx, func() {
		stop()
		cancel()
		s.mu.Lock()
		s.pending--
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}, true
}

func (a *Service) Shutdown(ctx context.Context) bool {
	s := a
	s.mu.Lock()
	s.closing = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.Close()
	}
	for {
		s.mu.Lock()
		pending, changed := s.pending, s.changed
		s.mu.Unlock()
		if pending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
	return s.manager == nil || s.manager.Shutdown(ctx)
}
