package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	featurepools "github.com/ydtg1993/papa/v4/admin/accountpools"
	featureaccounts "github.com/ydtg1993/papa/v4/admin/accounts"
	featurecontrol "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestAccountNavigationHasOneOwnedEntry(t *testing.T) {
	a := &App{Config: &config.Config{}}
	a.registerBuiltinPages()
	var found bool
	for _, p := range a.pages {
		if p.Key == "account-pools" {
			found = true
			if len(p.Owns) != 1 || p.Owns[0] != "page:accounts" {
				t.Fatalf("accounts must belong to the pool navigation entry: %+v", p)
			}
		}
	}
	if !found {
		t.Fatal("account pool entry missing")
	}
}

func TestAccountEntryUsesDeclaredSitesBeforeOnboarding(t *testing.T) {
	a, _ := runtimeTestApp(t)
	for name, sites := range map[string][]string{"accounts": a.accountsHandler().Sites(), "pools": a.accountPoolsHandler().Sites()} {
		if len(sites) != 1 || sites[0] != "fixture" {
			t.Fatalf("%s lost declared site without an onboarding service: %v", name, sites)
		}
	}
}

// Exercise the shipped page scripts against synthetic HTTP responses, including
// the unconfigured path that previously returned silently on every add click.
func TestAccountEntryRealBrowserSiteSelectionAndUnavailableFeedback(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	var configured atomic.Bool
	var writes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			writes.Add(1)
			var input struct {
				Profile map[string]any `json:"profile"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			input.Profile["revision"] = 1
			_ = json.NewEncoder(w).Encode(input.Profile)
			return
		}
		providers := []any{}
		if configured.Load() {
			providers = []any{
				map[string]any{"site": "password", "label": "密码站", "methods": []any{map[string]any{"id": "password", "label": "账号密码", "kind": "password", "fields": []any{map[string]any{"name": "password", "label": "密码", "secret": true}}}}},
				map[string]any{"site": "scan", "label": "扫码站", "methods": []any{map[string]any{"id": "scan", "label": "扫码授权", "kind": "qr", "fields": nil}}},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"configured": configured.Load(), "sites": []string{"todo", "password", "scan"}, "providers": providers, "profiles": []any{}, "message": "尚未施工"})
	})
	mux.HandleFunc("/api/account-pools", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			writes.Add(1)
			http.Error(w, "unexpected write", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"configured": configured.Load(), "sites": []string{"todo", "password", "scan"}, "pools": []any{}, "accounts": []any{
			map[string]any{"key": map[string]string{"site": "password", "id": "a"}},
			map[string]any{"key": map[string]string{"site": "scan", "id": "b"}},
		}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><body><div id="root"></div><script>
window.pages={};window.Papa={page:(k,f)=>pages[k]=f};window.apiRequest=(u,o)=>fetch(u,o);window.confirm=()=>true;
window.openPage=(key)=>{if(window.active){if(typeof active==='function')active();else if(active.destroy)active.destroy()}let root=document.getElementById('root');root.replaceChildren();window.active=pages[key](root)};
window.click=(text)=>{let b=Array.from(document.querySelectorAll('button')).find(x=>x.textContent===text);if(!b)throw Error('missing '+text);b.click()};
window.pick=(label,value)=>{let s=document.querySelector('select[aria-label="'+label+'"]');if(!s)throw Error('missing '+label);s.value=value;s.dispatchEvent(new Event('change'))};
</script><script>`+featurecontrol.Script+featurepools.Script+featureaccounts.Script+`</script><script>openPage('accounts')</script></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true, NoSandbox: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error {
		page, err := b.NewPage(ctx, srv.URL)
		if err != nil {
			return err
		}
		defer page.Close()
		eval := func(js string) error { _, err := page.Eval(js); return err }
		wait := func(js string) error {
			if err := page.Timeout(5 * time.Second).Wait(rod.Eval(js)); err != nil {
				body, _ := page.Eval(`()=>document.body.innerText`)
				return fmt.Errorf("%s: %w; page=%v", js, err, body)
			}
			return nil
		}
		if err := wait(`()=>document.getElementById('root').textContent.includes('账号池接入尚未施工')`); err != nil {
			return err
		}
		if err := eval(`()=>{if(Array.from(document.querySelectorAll('button')).some(b=>['账号','池设置'].includes(b.textContent)))throw Error('split account page remained');click('创建池');pick('账号池所属站点','todo');let d=document.querySelector('dialog');if(!d.textContent.includes('尚未施工')||!Array.from(d.querySelectorAll('button')).find(b=>b.textContent==='创建并管理账号').disabled)throw Error('unconfigured pool enabled');click('取消');click('站点账号')}`); err != nil {
			return err
		}
		if err := wait(`()=>document.querySelector('dialog')?.textContent.includes('新增账号')`); err != nil {
			return err
		}
		if err := eval(`()=>{click('新增账号');pick('账号所属站点','todo');if(!document.querySelector('dialog').textContent.includes('站点「todo」的账号登录接入尚未施工'))throw Error('unconfigured account enabled')}`); err != nil {
			return err
		}
		if writes.Load() != 0 {
			return fmt.Errorf("unconfigured UI wrote %d times", writes.Load())
		}
		configured.Store(true)
		if err := eval(`()=>{document.querySelector('dialog .rt-modal-close').click();click('刷新');click('站点账号')}`); err != nil {
			return err
		}
		if err := wait(`()=>document.querySelector('dialog')?.textContent.includes('新增账号')`); err != nil {
			return err
		}
		if err := eval(`()=>{click('新增账号');pick('账号所属站点','password');let d=document.querySelector('dialog');if(!d.querySelector('input[type=password]')||!d.textContent.includes('登录并验证'))throw Error('login was not combined');pick('账号所属站点','scan');if(d.querySelector('input[type=password]'))throw Error('cross-site fields retained');document.querySelector('dialog .rt-modal-close').click();click('创建池');pick('账号池所属站点','password');let options=Array.from(document.querySelectorAll('dialog select option')).map(x=>x.value);if(!options.includes('a')||options.includes('b'))throw Error('cross-site pool members');}`); err != nil {
			return err
		}
		return nil

	})
	if err != nil {
		t.Fatal(err)
	}
}
