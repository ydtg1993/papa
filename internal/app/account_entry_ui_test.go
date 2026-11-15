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
		steps := []string{
			`()=>document.getElementById('root').textContent.includes('尚未施工')`,
			`()=>{click('新增账号');pick('账号所属站点','todo');return document.querySelector('.rt-drawer').textContent.includes('站点「todo」的账号登录接入尚未施工') && !document.querySelector('.rt-drawer input')}`,
			`()=>{click('关闭');click('池设置');return true}`,
			`()=>document.getElementById('root').textContent.includes('账号池接入尚未施工')`,
			`()=>{click('创建池');pick('账号池所属站点','todo');return document.querySelector('.rt-drawer').textContent.includes('站点「todo」') && Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='继续设置池').disabled}`,
		}
		for _, js := range steps {
			if err := wait(js); err != nil {
				return err
			}
		}
		if writes.Load() != 0 {
			return fmt.Errorf("unconfigured UI submitted %d writes", writes.Load())
		}
		configured.Store(true)
		if err := eval(`()=>{click('取消');click('账号')}`); err != nil {
			return err
		}
		if err := wait(`()=>document.getElementById('root').textContent.includes('先保存资料')`); err != nil {
			return err
		}
		if err := eval(`()=>{click('新增账号');pick('账号所属站点','todo');if(!document.querySelector('.rt-drawer').textContent.includes('尚未施工'))throw Error('partially integrated site enabled');pick('账号所属站点','password');let d=document.querySelector('.rt-drawer');if(!d.textContent.includes('账号密码')||d.textContent.includes('扫码授权'))throw Error('wrong methods');let inputs=d.querySelectorAll('input');inputs[0].value='new';inputs[1].value='New account';click('保存资料')}`); err != nil {
			return err
		}
		if err := wait(`()=>document.querySelector('.rt-drawer input[type=password]')!==null`); err != nil {
			return err
		}
		if writes.Load() != 1 {
			return fmt.Errorf("expected one draft write, got %d", writes.Load())
		}
		if err := eval(`()=>{click('关闭');click('新增账号');pick('账号所属站点','scan');let d=document.querySelector('.rt-drawer');if(!d.textContent.includes('扫码授权')||d.textContent.includes('账号密码'))throw Error('cross-site methods');click('关闭');click('池设置')}`); err != nil {
			return err
		}
		if err := wait(`()=>document.getElementById('root').textContent.includes('疑似失效先核验')`); err != nil {
			return err
		}
		return eval(`()=>{click('创建池');pick('账号池所属站点','todo');if(!document.querySelector('.rt-drawer').textContent.includes('站点「todo」的账号登录接入尚未施工'))throw Error('missing empty member guidance');pick('账号池所属站点','password');click('继续设置池');let d=document.querySelector('.rt-drawer');let site=d.querySelector('input');if(!site.disabled||site.value!=='password')throw Error('pool site can drift');let options=Array.from(d.querySelectorAll('select option')).map(x=>x.value);if(!options.includes('a')||options.includes('b'))throw Error('cross-site pool members')}`)
	})
	if err != nil {
		t.Fatal(err)
	}
}
