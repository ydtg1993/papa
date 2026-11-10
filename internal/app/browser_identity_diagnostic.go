package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

type identityCheck struct {
	Field  string `json:"field"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}
type identityDiagnosticSummary struct {
	ID                 string                   `json:"id"`
	At                 time.Time                `json:"at"`
	Reference          identity.Reference       `json:"reference"`
	Subject            identityruntime.Subject  `json:"subject"`
	OK                 bool                     `json:"ok"`
	Runtime            identity.RuntimeIdentity `json:"runtime"`
	CheckerVersion     string                   `json:"checker_version"`
	OSVersion          string                   `json:"os_version,omitempty"`
	EnvironmentChanged bool                     `json:"environment_changed"`
}
type identityDiagnosticReport struct {
	identityDiagnosticSummary
	Title        string                    `json:"title"`
	Profile      identity.Profile          `json:"profile"`
	Resolved     *identity.ResolvedProfile `json:"resolved,omitempty"`
	FirstHeaders map[string]string         `json:"first_headers"`
	FirstScript  *identity.Observation     `json:"first_script,omitempty"`
	Observation  *identity.Observation     `json:"observation,omitempty"`
	Checks       []identityCheck           `json:"checks"`
	Limitations  []string                  `json:"limitations"`
	Error        string                    `json:"error,omitempty"`
}

// identityFixture permits one random path on one loopback listener. Responses
// contain only framework-owned code and identity header names, never credentials.
type identityFixture struct {
	server   *http.Server
	listener net.Listener
	URL      string
	mu       sync.Mutex
	headers  map[string]string
}

func newIdentityFixture() (*identityFixture, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &identityFixture{listener: l, headers: map[string]string{}}
	path := "/" + uuid.NewString()
	nonce := uuid.NewString()
	f.URL = "http://" + l.Addr().String() + path
	f.server = &http.Server{ReadHeaderTimeout: 3 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	f.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Host != l.Addr().String() || r.URL.Path != path || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		if len(f.headers) == 0 {
			for _, h := range []string{"User-Agent", "Accept-Language", "Sec-CH-UA", "Sec-CH-UA-Mobile", "Sec-CH-UA-Platform"} {
				f.headers[h] = r.Header.Get(h)
			}
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		fmt.Fprint(w, "<!doctype html><html><head><meta charset=utf-8><script nonce=\""+nonce+"\">window.__papa_first=("+identity.FirstScript+")();</script><title>配置一致性检查</title></head><body>本地合成页</body></html>")
	})
	go func() { _ = f.server.Serve(l) }()
	return f, nil
}
func (f *identityFixture) close() { _ = f.server.Close() }
func (f *identityFixture) firstHeaders() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.headers {
		out[k] = v
	}
	return out
}

func (a *App) diagnosticIdentityConfig(s identityruntime.Subject) (browser.PoolConfig, []string, error) {
	if s.Kind == "account" {
		if m := a.Engine.GetAccountManager(); m != nil {
			return m.DiagnosticBrowserConfig(account.Key{Site: s.Site, ID: s.Key})
		}
		return browser.PoolConfig{}, nil, errors.New("未配置账号管理器")
	}
	pool := a.Engine.GetBrowserPool()
	if pool == nil {
		return browser.PoolConfig{}, nil, errors.New("未配置公共浏览器池")
	}
	cfg, omitted := pool.DiagnosticConfig()
	return cfg, omitted, nil
}

func (a *App) runIdentityDiagnostic(ctx context.Context, p identity.Profile, s identityruntime.Subject) (*identityDiagnosticReport, error) {
	cfg, omitted, err := a.diagnosticIdentityConfig(s)
	if err != nil {
		return nil, err
	}
	r := runIdentityDiagnosticWithConfig(ctx, p, s, cfg, omitted)
	a.identityState.mu.Lock()
	for _, old := range a.identityState.reports {
		if old.Reference == r.Reference && old.Subject == r.Subject && old.OK && r.Observation != nil {
			r.EnvironmentChanged = old.Runtime != r.Runtime || old.CheckerVersion != r.CheckerVersion || old.OSVersion != r.OSVersion
			break
		}
	}
	a.identityState.reports = append([]identityDiagnosticSummary{r.identityDiagnosticSummary}, a.identityState.reports...)
	if len(a.identityState.reports) > 128 {
		a.identityState.reports = a.identityState.reports[:128]
	}
	a.identityState.mu.Unlock()
	if a.Logger != nil && a.Logger.Sys != nil {
		raw, _ := json.Marshal(r.identityDiagnosticSummary)
		a.Logger.Sys.Infof("browser identity diagnostic %s", raw)
	}
	return r, nil
}

func runIdentityDiagnosticWithConfig(ctx context.Context, p identity.Profile, s identityruntime.Subject, cfg browser.PoolConfig, omitted []string) *identityDiagnosticReport {
	r := &identityDiagnosticReport{identityDiagnosticSummary: identityDiagnosticSummary{ID: uuid.NewString(), At: time.Now(), Reference: p.Reference(), Subject: s}, Title: "配置一致性检查", Profile: p.Clone(), Checks: []identityCheck{}, Limitations: append([]string{
		"仅检查受管页面主文档；弹窗、跨进程 iframe、Worker 和原始 Rod 页面未验证。",
		"未加载 Cookie、Token、localStorage，未调用登录回调，未验证目标站认证或网络出口信誉。",
		"HTTP 身份头未联动；Go HTTP 的 TLS/HTTP2 指纹未模拟。",
		"字体仅为有限样本；Canvas/WebGL 随系统、驱动和浏览器环境变化。",
		"历史诊断仅保留本进程最近 128 份摘要；环境变化不等同于配置错误。",
	}, omitted...)}
	fail := func(err error) *identityDiagnosticReport { r.Error = redactRod(err.Error(), nil); return r }
	if err := p.Validate(); err != nil {
		return fail(err)
	}
	fixture, err := newIdentityFixture()
	if err != nil {
		return fail(err)
	}
	defer fixture.close()
	root, err := os.MkdirTemp("", "papa-identity-diagnostic-")
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			r.OK = false
			r.Error = "诊断临时目录清理失败：" + redactRod(err.Error(), nil)
		}
	}()
	origin := "http://" + fixture.listener.Addr().String()
	// A fresh pool owns this exact-origin grant. It cannot request any public or
	// other private destination, including redirects or browser background traffic.
	cfg.Network = egress.Config{PrivateOrigins: []string{origin}, AllowedOrigins: []string{origin}}
	cfg.TempDir = root
	cfg.Size = 1
	cfg.DirectSize = 0
	cfg.MaxIdleTime = 0
	cfg.SelfCheck = false
	cfg.ProxyManager = nil
	cfg.DefaultCookies = nil
	pool, err := browser.NewDiagnosticPool(cfg)
	if err != nil {
		return fail(err)
	}
	defer pool.Close()
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error {
		resolved, err := b.ResolveIdentity(ctx, p)
		if err != nil {
			return err
		}
		r.Resolved = resolved
		return b.WithNavigatedPage(ctx, fixture.URL, browser.PageOptions{Identity: resolved}, func(page *rod.Page) error {
			first, err := page.Eval(`() => window.__papa_first`)
			if err != nil {
				return err
			}
			var o identity.Observation
			if err = first.Value.Unmarshal(&o); err != nil {
				return err
			}
			r.FirstScript = &o
			r.Observation, err = browser.ObserveIdentity(page)
			if err != nil {
				return err
			}
			r.Runtime = r.Observation.Runtime
			r.CheckerVersion = r.Observation.CheckerVersion
			if r.Observation.UserAgentMetadata != nil {
				r.OSVersion = r.Observation.UserAgentMetadata.PlatformVersion
			}
			r.FirstHeaders = fixture.firstHeaders()
			r.Checks = compareIdentityDiagnostic(resolved, r.FirstHeaders, &o, r.Observation)
			for _, limitation := range omitted {
				if strings.HasPrefix(limitation, "identity conflict:") {
					r.Checks = append(r.Checks, identityCheck{"生产配置", "failed", limitation})
				}
			}
			r.OK = true
			for _, c := range r.Checks {
				if c.Status != "passed" {
					r.OK = false
				}
			}
			return nil
		})
	})
	if err != nil {
		return fail(err)
	}
	return r
}

func compareIdentityDiagnostic(r *identity.ResolvedProfile, h map[string]string, first, observed *identity.Observation) []identityCheck {
	checks := []identityCheck{}
	check := func(name string, ok bool) {
		status := "passed"
		reason := ""
		if !ok {
			status = "failed"
			reason = "声明、解析值与实际值不一致"
		}
		checks = append(checks, identityCheck{name, status, reason})
	}
	p := r.Profile
	check("首个请求 User-Agent", h["User-Agent"] == r.UserAgent)
	langs := []string{}
	for _, v := range strings.Split(h["Accept-Language"], ",") {
		langs = append(langs, strings.TrimSpace(strings.SplitN(v, ";", 2)[0]))
	}
	check("首个请求 Accept-Language", reflect.DeepEqual(langs, p.Languages))
	for _, sample := range []struct {
		name string
		o    *identity.Observation
	}{{"首段脚本", first}, {"加载后", observed}} {
		o := sample.o
		name := sample.name + " "
		check(name+"UA/platform", o.UserAgent == r.UserAgent && o.Platform == r.Platform)
		check(name+"语言/locale", reflect.DeepEqual(o.Languages, p.Languages) && strings.EqualFold(o.Language, p.Languages[0]) && strings.EqualFold(o.Locale, p.Locale))
		check(name+"时区", o.Timezone == p.Timezone)
		check(name+"屏幕/viewport/DPR/touch", o.Viewport == p.Viewport && o.Screen == p.Screen && o.DeviceScaleFactor == p.DeviceScaleFactor && o.MaxTouchPoints == p.MaxTouchPoints)
		if !o.ClientHintsAvailable || o.UserAgentMetadata == nil {
			checks = append(checks, identityCheck{name + "Client Hints", "unavailable", "当前上下文未提供 Client Hints"})
		} else {
			check(name+"Client Hints", o.UserAgentMetadata.Platform == r.UserAgentMetadata.Platform && o.UserAgentMetadata.Mobile == r.UserAgentMetadata.Mobile && compatibleCHBrands(o.UserAgentMetadata.Brands, r.UserAgentMetadata.Brands))
		}
	}
	if !observed.ClientHintsAvailable || observed.UserAgentMetadata == nil || observed.ClientHintsReason != "" || len(observed.UserAgentMetadata.FullVersionList) == 0 {
		checks = append(checks, identityCheck{"加载后高熵 Client Hints", "unavailable", "未获得完整高熵 Client Hints，不能确认一致"})
	} else {
		a, e := observed.UserAgentMetadata, r.UserAgentMetadata
		check("加载后高熵 Client Hints", a.Architecture == e.Architecture && a.Bitness == e.Bitness && a.Model == e.Model && a.PlatformVersion == e.PlatformVersion && a.Wow64 == e.Wow64 && compatibleCHBrands(a.FullVersionList, e.FullVersionList))
	}
	if h["Sec-CH-UA"] == "" || h["Sec-CH-UA-Platform"] == "" {
		checks = append(checks, identityCheck{"首个请求 Client Hints", "unavailable", "浏览器未发送低熵 Client Hints"})
	} else {
		check("首个请求 Client Hints", strings.Trim(h["Sec-CH-UA-Platform"], "\"") == r.UserAgentMetadata.Platform && h["Sec-CH-UA-Mobile"] == map[bool]string{true: "?1", false: "?0"}[r.UserAgentMetadata.Mobile] && headerCHBrandsMatch(h["Sec-CH-UA"], r.UserAgentMetadata.Brands))
	}
	if p.Adapter.Enabled {
		check("适配器注册", observed.Adapter.Registered)
		check("适配器首次执行", first.AdapterGetter == "papaIdentityWebdriverV1" && !first.Webdriver)
		check("适配器加载后执行", observed.Adapter.Executed && observed.Adapter.Verified)
	}
	return checks
}

var chBrandPair = regexp.MustCompile(`"([^"\\]+)"\s*;\s*v="([0-9.]+)"`)
var chBrandLetters = regexp.MustCompile(`[^a-z]`)

func compatibleCHBrands(actual, expected []*proto.EmulationUserAgentBrandVersion) bool {
	stable := func(list []*proto.EmulationUserAgentBrandVersion) map[string]string {
		out := map[string]string{}
		for _, v := range list {
			if v != nil && chBrandLetters.ReplaceAllString(strings.ToLower(v.Brand), "") != "notabrand" {
				out[v.Brand] = v.Version
			}
		}
		return out
	}
	a, e := stable(actual), stable(expected)
	return len(e) > 0 && reflect.DeepEqual(a, e)
}
func headerCHBrandsMatch(header string, expected []*proto.EmulationUserAgentBrandVersion) bool {
	actual := []*proto.EmulationUserAgentBrandVersion{}
	for _, m := range chBrandPair.FindAllStringSubmatch(header, -1) {
		actual = append(actual, &proto.EmulationUserAgentBrandVersion{Brand: m[1], Version: m[2]})
	}
	return compatibleCHBrands(actual, expected)
}
