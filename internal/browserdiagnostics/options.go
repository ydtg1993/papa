package browserdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"time"
)

// Resources supplies only browser and identity resources, never execution state.
type Resources interface {
	GetBrowserPool() *browser.Pool
	GetAccountManager() *account.Manager
	GetProxy() *proxy.Manager
}
type Options struct {
	HasSite   func(string) bool
	Config    *config.BrowserConfig
	Resources Resources
	Log       func(string, ...any)
}

var ErrBusy = errors.New("已有浏览器诊断正在运行")
var ErrClosing = errors.New("浏览器身份管理正在停止")

// Configure binds dependencies exactly once before use. The zero value is valid.
func (a *Service) Configure(options Options) { a.once.Do(func() { a.options = options }) }

// Initialize constructs the identity manager; ownership transfers to the service.
func (a *Service) Initialize(store identityruntime.Store) error {
	m, err := identityruntime.NewManager(store, identityruntime.Options{
		Resolve: a.resolveBrowserIdentity, Verify: a.verifyBrowserIdentity, VerifyTimeout: 90 * time.Second,
	})
	if err != nil {
		return err
	}
	a.SetManager(m)
	return nil
}

// SetManager attaches a manager during startup, before serving operations.
func (a *Service) SetManager(m *identityruntime.Manager) { a.manager = m }
func (a *Service) Manager() *identityruntime.Manager     { return a.manager }
func (a *Service) Diagnose(parent context.Context, ref identity.Reference, subject identityruntime.Subject) (*IdentityDiagnosticReport, error) {
	ctx, done, ok := a.Begin(parent)
	if !ok {
		return nil, ErrClosing
	}
	defer done()
	if !a.diagnostic.TryLock() {
		return nil, ErrBusy
	}
	defer a.diagnostic.Unlock()
	if a.manager == nil {
		return nil, errors.New("浏览器身份管理尚未初始化")
	}
	p, err := a.manager.Store().GetProfile(ctx, ref)
	if err != nil {
		return nil, err
	}
	return a.runIdentityDiagnostic(ctx, p, subject)
}

// Rod participates in the shared cancellation/drain boundary and admission gate.
// Its launcher, temporary profile and log stay local to this execution.
func (a *Service) Rod(parent context.Context) (RodReport, error) {
	ctx, done, ok := a.Begin(parent)
	if !ok {
		return RodReport{}, ErrClosing
	}
	defer done()
	if !a.diagnostic.TryLock() {
		return RodReport{}, ErrBusy
	}
	defer a.diagnostic.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	report := a.runRodDiagnostic(ctx)
	if a.options.Log != nil {
		data, _ := json.Marshal(report)
		a.options.Log("rod diagnostics %s", data)
	}
	return report, nil
}
