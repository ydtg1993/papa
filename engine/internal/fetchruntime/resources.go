package fetchruntime

import (
	"context"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"sync"
)

// Resources owns fetch-resource registration. Calls snapshot dependencies under
// the lock; network work and user callbacks never run while it is held.
type Resources struct {
	mu       sync.RWMutex
	accounts *account.Manager
	identity *identityruntime.Manager
	browser  *browser.Pool
	html     *htmlfetch.Client
}

func (r *Resources) SetAccount(m *account.Manager) { r.mu.Lock(); defer r.mu.Unlock(); r.accounts = m }
func (r *Resources) Account() *account.Manager     { r.mu.RLock(); defer r.mu.RUnlock(); return r.accounts }
func (r *Resources) SetIdentity(m *identityruntime.Manager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.identity = m
}
func (r *Resources) Identity() *identityruntime.Manager {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.identity
}
func (r *Resources) SetBrowser(m *browser.Pool)  { r.mu.Lock(); defer r.mu.Unlock(); r.browser = m }
func (r *Resources) Browser() *browser.Pool      { r.mu.RLock(); defer r.mu.RUnlock(); return r.browser }
func (r *Resources) SetHTML(m *htmlfetch.Client) { r.mu.Lock(); defer r.mu.Unlock(); r.html = m }
func (r *Resources) HTML() *htmlfetch.Client     { r.mu.RLock(); defer r.mu.RUnlock(); return r.html }

// RenderResult separates resource execution from Engine trace/archive policy.
// Account content must never be offered to the raw archive.
type RenderResult struct {
	HTML, URL, ArchiveURL string
	Account, Archive      bool
}

func BrowserProxyError(ctx context.Context) error {
	if addr := core.ProxyURLFrom(ctx); addr != "" {
		return fmt.Errorf("浏览器路径不支持逐请求代理地址（%s）：代理是浏览器实例级设置，Chrome 不提供逐请求改路由。要给这个站换出口，用 pool 上的代理配置、或一站一进程", addr)
	}
	return nil
}
func (r *Resources) Render(ctx context.Context, rawURL string, opts browser.PageOptions) (RenderResult, error) {
	var out RenderResult
	if a, ok := account.AttemptFromContext(ctx); ok {
		out.Account = true
		m := r.Account()
		if m == nil {
			return out, account.ErrPoolUnknown
		}
		err := m.NavigatePoolPage(ctx, a.Pool, rawURL, opts, func(page *rod.Page) error {
			var err error
			out.HTML, err = page.HTML()
			if err != nil {
				return err
			}
			info, err := page.Info()
			if err == nil {
				out.URL = info.URL
			}
			return err
		})
		return out, err
	}
	if err := BrowserProxyError(ctx); err != nil {
		return out, err
	}
	pool := r.Browser()
	if pool == nil {
		return out, fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}
	bw, err := pool.Get(ctx)
	if err != nil {
		return out, fmt.Errorf("acquire browser: %w", err)
	}
	defer pool.Put(bw)
	page, err := bw.NewPageWithOptions(ctx, rawURL, opts)
	if err != nil {
		return out, fmt.Errorf("open page %s: %w", rawURL, err)
	}
	defer page.Close()
	html, err := page.HTML()
	if err != nil {
		return out, fmt.Errorf("read html of %s: %w", rawURL, err)
	}
	info, infoErr := page.Info()
	out.Archive = true
	out.HTML = html
	out.ArchiveURL = rawURL
	if infoErr == nil && info.URL != "" {
		out.ArchiveURL = info.URL
	}
	if infoErr != nil {
		return out, fmt.Errorf("get page info of %s: %w", rawURL, infoErr)
	}
	out.URL = info.URL
	return out, nil
}

// PrepareAccount does not consume a request permit or change durable task state.
func (r *Resources) PrepareAccount(ctx context.Context, site, pool string) (context.Context, error) {
	if pool == "" {
		return ctx, nil
	}
	m := r.Account()
	if m == nil || m.Pools() == nil {
		return nil, fmt.Errorf("account pools are not configured")
	}
	a, err := m.Pools().Acquire(ctx, account.PoolKey{Site: site, Name: pool}, "")
	if err != nil {
		return nil, err
	}
	return account.WithAttempt(ctx, a), nil
}
