package app

import (
	"context"
	"errors"
	"net/http"

	"github.com/ydtg1993/papa/v4/admin/accounts"
	"github.com/ydtg1993/papa/v4/pkg/account/onboarding"
)

var accountsScript = accounts.Script

// UseAccountOnboarding installs a business-constructed service before Run.
// The business must explicitly supply encryption, store and site Providers.
func (a *App) UseAccountOnboarding(s *onboarding.Service) error {
	if s == nil || a.Engine == nil {
		return onboarding.ErrNotConfigured
	}
	if a.accountOnboarding == s {
		return nil
	}
	if a.accountOnboarding != nil {
		return errors.New("account onboarding service already installed")
	}
	if manager := a.Engine.GetAccountManager(); manager != nil && manager != s.Manager() {
		return errors.New("account onboarding manager differs from engine manager")
	}
	a.Engine.SetAccountManager(s.Manager())
	s.WrapContext(func(ctx context.Context, site string) context.Context { return a.Engine.RuntimeContext(ctx, site) })
	s.SetEventListener(a.Engine.NotifyAccountHealthEvent)
	a.accountOnboarding = s
	return nil
}

func (a *App) accountsHandler() *accounts.Handler {
	h := &accounts.Handler{Audit: a.runtimeAudit, Runtime: a.Engine.AccountStatus}
	if a.accountOnboarding != nil {
		h.Service = a.accountOnboarding
	}
	return h
}
func (a *App) accountsAPI(w http.ResponseWriter, r *http.Request) { a.accountsHandler().Accounts(w, r) }
func (a *App) accountsRemoteAPI(w http.ResponseWriter, r *http.Request) {
	a.accountsHandler().Remote(w, r)
}
