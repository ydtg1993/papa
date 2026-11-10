package engine

import (
	"context"
	"errors"

	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

// IdentityManager shares the account registry's identity runtime. Install it
// before starting workers; it never changes account management versions.
func (e *Engine) IdentityManager() *identityruntime.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.identityManager
}
func (e *Engine) SetIdentityManager(v *identityruntime.Manager) {
	e.mu.Lock()
	e.identityManager = v
	e.mu.Unlock()
}

// WithSessionIdentity pins an explicit business session key. htmlfetch.Session
// remains a CookieJar and does not supply or infer this key.
func (e *Engine) WithSessionIdentity(ctx context.Context, site, key string, fn func(context.Context, *identity.ResolvedProfile) error) error {
	m := e.IdentityManager()
	if m == nil {
		return errors.New("browser identity runtime is not configured")
	}
	return m.WithSession(e.RuntimeContext(ctx, site), site, key, fn)
}
