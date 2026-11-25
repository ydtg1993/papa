package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	featurepools "github.com/ydtg1993/papa/v4/admin/accountpools"
	featureaccounts "github.com/ydtg1993/papa/v4/admin/accounts"
	"github.com/ydtg1993/papa/v4/admin/auth"
	featurecontrol "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestAccountModalRealBrowserCreateLoginJoinRemoveAndLayout(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	a := accountsTestApp(t)
	pools, err := account.NewPools(context.Background(), a.Engine.GetAccountManager(), account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	a.runtimeState.pools = pools
	if err = a.runtimeState.service.AttachPools(pools); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		a.accountsHandler().Accounts(w, r.WithContext(auth.WithOperator(r.Context(), "modal-operator")))
	})
	var rejectJoin atomic.Bool
	mux.HandleFunc("/api/account-pools", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && rejectJoin.CompareAndSwap(true, false) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			fmt.Fprint(w, `{"error":"池版本已变化，请核对后重试"}`)
			return
		}
		a.accountPoolsHandler().PoolsAPI(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>body{font:14px system-ui;background:#f6f8f7;color:#202e33;margin:32px}button,select,input{border:1px solid #d3dce0;border-radius:4px;background:#fff;color:inherit}.rt-status{color:#657078}h1{font-size:22px}#root{padding:24px;background:#eef3f5;min-height:360px}button{cursor:pointer}:root{--primary:#ffd83d;--border-color:#d3dce0;--card:#fff}</style><h1>账号池</h1><div id="root"></div><script>window.pages={};window.Papa={page:(k,f)=>pages[k]=f};window.apiRequest=(u,o)=>fetch(u,o);window.confirm=()=>true;window.click=(text)=>{let scope=document.querySelector('dialog[open]')||document;let b=Array.from(scope.querySelectorAll('button')).find(b=>b.textContent===text);if(!b)throw Error('missing '+text);b.click()};window.pick=(label,value)=>{let s=document.querySelector('select[aria-label="'+label+'"]');s.value=value;s.dispatchEvent(new Event('change'))};</script><script>`+featurecontrol.Script+featurepools.Script+featureaccounts.Script+`</script><script>window.active=pages['account-pools'](document.getElementById('root'));</script></html>`)
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
		if err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: 1280, Height: 860, DeviceScaleFactor: 1}); err != nil {
			return err
		}
		eval := func(js string) error { _, err := page.Eval(js); return err }
		wait := func(js string) error {
			if err := page.Timeout(6 * time.Second).Wait(rod.Eval(js)); err != nil {
				body, _ := page.Eval(`()=>document.body.innerText`)
				return fmt.Errorf("%s: %w; %v", js, err, body)
			}
			return nil
		}
		shot := func(name string) error {
			dir := os.Getenv("PAPA_ACCOUNT_SCREENSHOT_DIR")
			if dir == "" {
				return nil
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			png, err := page.Screenshot(false, nil)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, name+".png"), png, 0600)
		}
		if err = wait(`()=>document.body.textContent.includes('创建第一个账号池')`); err != nil {
			return err
		}
		if err = eval(`()=>{let heights=Array.from(document.querySelectorAll('#root > .rt-toolbar button,#root > .rt-toolbar select')).map(x=>x.getBoundingClientRect().height);if(Math.max(...heights)-Math.min(...heights)>1)throw Error('toolbar heights differ '+heights);click('创建池');pick('账号池所属站点','fixture');let name=Array.from(document.querySelectorAll('dialog label')).find(l=>l.textContent.startsWith('池名')).querySelector('input');name.value='main';name.dispatchEvent(new Event('input'));click('创建并管理账号')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog[open]')?.textContent.includes('本站还没有账号')`); err != nil {
			return err
		}
		if err = eval(`()=>{click('新增账号');let d=document.querySelector('dialog');if(d.querySelector('select[aria-label="账号所属站点"]').value!=='fixture'||!d.querySelector('select').disabled)throw Error('pool site not fixed');d.querySelector('[data-login-field=password]').value='private-password';}`); err != nil {
			return err
		}
		if err = shot("account-add-desktop"); err != nil {
			return err
		}
		if err = eval(`()=>{document.querySelector('[data-login-field=password]').value='incorrect';click('登录并验证')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog')?.textContent.includes('登录未完成')`); err != nil {
			return err
		}
		profiles, err := a.accountsHandler().Service.Profiles(ctx)
		if err != nil || len(profiles) != 1 {
			return fmt.Errorf("failed login did not retain exactly one draft: %v", err)
		}
		if err = eval(`()=>click('重新登录')`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog[open] [data-login-field=password]')!=null`); err != nil {
			return err
		}
		if err = eval(`()=>{document.querySelector('[data-login-field=password]').value='private-password';click('开始登录')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog')?.textContent.includes('确认身份并加入此池')`); err != nil {
			return err
		}
		rejectJoin.Store(true)
		if err = eval(`()=>{if(document.body.innerText.includes('private-password'))throw Error('password leaked');click('确认身份并加入此池')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog .ac-feedback')?.textContent.includes('池版本已变化')`); err != nil {
			return err
		}
		beforeRetry, err := pools.Get(account.PoolKey{Site: "fixture", Name: "main"})
		if err != nil || len(beforeRetry.Members) != 0 {
			return fmt.Errorf("failed join was replayed: %v", err)
		}
		if err = eval(`()=>{if(!document.querySelector('dialog').textContent.includes('账号已保存并生效'))throw Error('pool failure lost account success');click('加入当前池')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog')?.textContent.includes('移出此池')`); err != nil {
			return err
		}
		snapshot, err := pools.Get(account.PoolKey{Site: "fixture", Name: "main"})
		if err != nil || len(snapshot.Members) != 1 {
			return fmt.Errorf("login not joined to pool: %v", err)
		}
		if err = shot("account-members-desktop"); err != nil {
			return err
		}
		if err = eval(`()=>{let d=document.querySelector('dialog'),r=d.getBoundingClientRect();if(Math.abs((r.left+r.right)/2-innerWidth/2)>2)throw Error('dialog not centered');if(getComputedStyle(d.querySelector('td')).verticalAlign!=='middle')throw Error('cells not centered');click('移出此池')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog')?.textContent.includes('加入此池')&&!document.querySelector('dialog')?.textContent.includes('移出此池')`); err != nil {
			return err
		}
		snapshot, err = pools.Get(snapshot.Key)
		if err != nil || len(snapshot.Members) != 0 {
			return fmt.Errorf("member removal failed: %v", err)
		}
		if err = eval(`()=>document.querySelector('dialog .rt-modal-close').click()`); err != nil {
			return err
		}
		if err = shot("account-pools-desktop"); err != nil {
			return err
		}
		if err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: 390, Height: 844, DeviceScaleFactor: 1, Mobile: true}); err != nil {
			return err
		}
		if err = eval(`()=>{click('管理账号')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog[open]')?.textContent.includes('加入此池')`); err != nil {
			return err
		}
		if err = eval(`()=>{click('新增账号')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('dialog[open]')?.querySelector('[data-login-field]')!=null`); err != nil {
			return err
		}
		if err = eval(`()=>{let r=document.querySelector('dialog').getBoundingClientRect();if(r.left<0||r.right>innerWidth+1||r.height>innerHeight)throw Error('mobile overflow');}`); err != nil {
			return err
		}
		if err = shot("account-add-mobile"); err != nil {
			return err
		}
		return eval(`()=>{active.destroy();if(document.querySelector('dialog'))throw Error('modal leaked after destroy');}`)
	})
	if err != nil {
		t.Fatal(err)
	}
}
