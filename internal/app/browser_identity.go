package app

import (
	"context"
	"errors"
	"github.com/go-rod/rod"
	"net/http"
	"sync"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
)

type browserIdentityState struct {
	manager *identityruntime.Manager
	mu      sync.Mutex
	reports []identityDiagnosticSummary
	closing bool
	pending int
	changed chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

// BrowserIdentities returns the single-instance identity service after Run's
// initialization. Merely reading this service never launches a browser.
func (a *App) BrowserIdentities() *identityruntime.Manager { return a.identityState.manager }

func (a *App) initBrowserIdentities() error {
	m, err := identityruntime.NewManager(identityruntime.NewGORMStore(a.DB), identityruntime.Options{
		Resolve: a.resolveBrowserIdentity, Verify: a.verifyBrowserIdentity, VerifyTimeout: 90 * time.Second,
	})
	if err != nil {
		return err
	}
	a.identityState.manager = m
	a.identityState.ctx, a.identityState.cancel = context.WithCancel(context.Background())
	a.Engine.SetIdentityManager(m)
	if accounts := a.Engine.GetAccountManager(); accounts != nil {
		accounts.SetIdentityManager(m)
	}
	return nil
}

func (a *App) resolveBrowserIdentity(ctx context.Context, s identityruntime.Subject, p identity.Profile) (resolved *identity.ResolvedProfile, err error) {
	if s.Kind == "account" {
		if m := a.Engine.GetAccountManager(); m != nil {
			return m.ResolveIdentity(ctx, s, p)
		}
		return nil, errors.New("未配置账号管理器")
	}
	pool := a.Engine.GetBrowserPool()
	if pool == nil {
		return nil, errors.New("未配置公共浏览器池")
	}
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error { var e error; resolved, e = b.ResolveIdentity(ctx, p); return e })
	return
}

func (a *App) verifyBrowserIdentity(ctx context.Context, s identityruntime.Subject, r *identity.ResolvedProfile) (func() error, error) {
	if s.Kind == "account" {
		if m := a.Engine.GetAccountManager(); m != nil {
			return m.VerifyIdentity(ctx, s, r)
		}
		return nil, errors.New("未配置账号管理器")
	}
	pool := a.Engine.GetBrowserPool()
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

func (a *App) identityView(ctx context.Context, site string) (map[string]any, error) {
	m := a.BrowserIdentities()
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
	a.identityState.mu.Lock()
	reports := make([]identityDiagnosticSummary, 0, len(a.identityState.reports))
	for _, report := range a.identityState.reports {
		if site == "" || report.Reference.Site == site {
			reports = append(reports, report)
		}
	}
	a.identityState.mu.Unlock()
	return map[string]any{"profiles": profiles, "bindings": bindings, "reports": reports, "sites": a.Engine.GetSiteStats()}, nil
}

type identityInput struct {
	Action    string                  `json:"action"`
	Expected  uint64                  `json:"expected"`
	Profile   identity.Profile        `json:"profile"`
	Subject   identityruntime.Subject `json:"subject"`
	Reference identity.Reference      `json:"reference"`
}

func (a *App) browserIdentitiesAPI(w http.ResponseWriter, r *http.Request) {
	ctx, done, ok := a.beginIdentityOperation(r.Context())
	if !ok {
		writeRuntimeJSON(w, 503, map[string]any{"error": "浏览器身份管理正在停止"})
		return
	}
	defer done()
	r = r.WithContext(ctx)
	if r.Method == http.MethodGet {
		view, err := a.identityView(r.Context(), r.URL.Query().Get("site"))
		if err != nil {
			identityAPIError(w, err)
			return
		}
		writeRuntimeJSON(w, 200, view)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input identityInput
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 64<<10), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "无效身份管理请求"})
		return
	}
	m := a.BrowserIdentities()
	if m == nil {
		identityAPIError(w, errors.New("浏览器身份管理尚未初始化"))
		return
	}
	site := input.Subject.Site
	if input.Action == "profile" {
		site = input.Profile.Site
	}
	if input.Action == "diagnose" {
		site = input.Reference.Site
	}
	if input.Action == "diagnose" && ((input.Subject.Site != "" && input.Subject.Site != site) || (input.Subject.Kind != "" && input.Subject.Kind != "session" && input.Subject.Kind != "account")) {
		writeRuntimeJSON(w, 400, map[string]any{"error": "诊断主体与档案站点不一致或类型无效"})
		return
	}
	if _, ok := a.Engine.Site(site); !ok {
		writeRuntimeJSON(w, 400, map[string]any{"error": "未知站点"})
		return
	}
	if input.Action != "profile" && input.Action != "diagnose" {
		if err := input.Subject.Validate(); err != nil {
			writeRuntimeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
	}
	if input.Subject.Kind == "account" {
		accounts := a.Engine.GetAccountManager()
		if accounts == nil {
			writeRuntimeJSON(w, 400, map[string]any{"error": "未配置账号管理器"})
			return
		}
		// Configuration lookup only: do not create a browser or authenticate here.
		found := false
		for _, status := range accounts.Status() {
			if status.Key == (account.Key{Site: input.Subject.Site, ID: input.Subject.Key}) {
				found = true
				break
			}
		}
		if !found {
			writeRuntimeJSON(w, 400, map[string]any{"error": "账号未配置浏览器或不存在"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	var result any
	var err error
	switch input.Action {
	case "profile":
		if err = input.Profile.Validate(); err != nil {
			writeRuntimeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		err = m.Store().AppendProfile(ctx, input.Profile, input.Expected)
		result = input.Profile
	case "stage":
		result, err = m.Stage(ctx, input.Subject, input.Reference, input.Expected)
	case "cancel":
		result, err = m.Cancel(ctx, input.Subject, input.Expected)
	case "reverify":
		result, err = m.Reverify(ctx, input.Subject, input.Expected)
	case "diagnose":
		if !rodDiagnosticMu.TryLock() {
			writeRuntimeJSON(w, 409, map[string]any{"error": "已有浏览器诊断正在运行"})
			return
		}
		defer rodDiagnosticMu.Unlock()
		var p identity.Profile
		p, err = m.Store().GetProfile(ctx, input.Reference)
		if err == nil {
			result, err = a.runIdentityDiagnostic(ctx, p, input.Subject)
		}
	default:
		writeRuntimeJSON(w, 400, map[string]any{"error": "未知操作"})
		return
	}
	a.runtimeAudit(r, "browser_identity", input.Action, site+":"+input.Subject.Kind+":"+input.Subject.Key, err)
	if err != nil {
		identityAPIError(w, err)
		return
	}
	writeRuntimeJSON(w, 200, map[string]any{"result": result})
}

func (a *App) beginIdentityOperation(parent context.Context) (context.Context, func(), bool) {
	s := &a.identityState
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, false
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

func (a *App) stopBrowserIdentities(ctx context.Context) bool {
	s := &a.identityState
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

func identityAPIError(w http.ResponseWriter, err error) {
	status := 503
	if errors.Is(err, identityruntime.ErrConflict) {
		status = 409
	}
	if errors.Is(err, identityruntime.ErrNotFound) {
		status = 404
	}
	if errors.Is(err, identityruntime.ErrVerification) {
		status = 422
	}
	if errors.Is(err, identity.ErrConflict) || errors.Is(err, identity.ErrIncompatible) {
		status = 422
	}
	writeRuntimeJSON(w, status, map[string]any{"error": redactRod(err.Error(), nil)})
}
