package app

import (
	"context"
	"database/sql/driver"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeUIRealBrowserDraftSaveConflictAndNavigation(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	a, fake := runtimeTestApp(t)
	manager := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	a.Engine.SetAccountManager(manager)
	pools, err := account.NewPools(context.Background(), manager, account.NewMemoryPoolStore(), account.PoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	a.runtimeState.pools = pools
	mux := http.NewServeMux()
	mux.HandleFunc("/api/runtime-policy", a.runtimePolicyAPI)
	mux.HandleFunc("/api/runtime-policy/history", a.runtimeHistoryAPI)
	mux.HandleFunc("/api/account-pools", a.accountPoolsAPI)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><meta charset="utf-8"><style>body{font:14px system-ui;margin:24px}.btn{padding:7px 12px;margin:3px;border:1px solid #ccd4d5;background:white;border-radius:5px}button{cursor:pointer}</style><div id="root"></div><script>window.pages={};window.Papa={page:(name,fn)=>pages[name]=fn};window.apiRequest=async function(url,o){return fetch(url,o)};</script><script>`+runtimeControlScript+accountPoolsScript+`</script><script>window.active=pages['crawl-control'](document.getElementById('root'));</script></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true, NoSandbox: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error {
		page, err := b.NewPage(ctx, srv.URL)
		if err != nil {
			return err
		}
		defer page.Close()
		if _, err = page.Timeout(8 * time.Second).Element(".rt-table tbody tr"); err != nil {
			return err
		}
		eval := func(js string) error { _, e := page.Eval(js); return e }
		if err = eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='调整').click();let row=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent.startsWith('最大请求并发'));let check=row.querySelector('input[type=checkbox]');check.checked=true;check.dispatchEvent(new Event('change'));let input=row.querySelector('input[type=number]');input.value='2';input.dispatchEvent(new Event('input'));Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='刷新').click();}`); err != nil {
			return err
		}
		if err = eval(`()=>{let row=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent.startsWith('最大请求并发'));if(row.querySelector('input[type=number]').value!=='2')throw new Error('poll overwrote draft');if(!document.querySelector('.rt-drawer').textContent.includes('1 秒～2 秒'))throw new Error('declaration delay range missing');Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='保存并生效').click();}`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>!document.querySelector('.rt-drawer')`)); err != nil {
			return err
		}
		// Re-open editor; concurrent external edit makes its revision stale.
		if err = eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='调整').click();let row=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent.startsWith('最大请求并发'));let input=row.querySelector('input[type=number]');input.value='3';input.dispatchEvent(new Event('input'));}`); err != nil {
			return err
		}
		w := runtimePost(a, `{"revision":1,"document":{"schema_version":1,"overrides":{"sites":{"fixture":{"weight":7,"request":{"max_inflight":2}}}}}}`)
		if w.Code != 200 {
			return fmt.Errorf("external edit %d", w.Code)
		}
		if err = eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='保存并生效').click()`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='核对最新值并保留本次字段修改')`)); err != nil {
			return err
		}
		if err = eval(`()=>{if(document.querySelector('.rt-field input[type=number]').value==='')throw new Error('draft cleared');Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='核对最新值并保留本次字段修改').click();Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='保存并生效').click();}`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>!document.querySelector('.rt-drawer')`)); err != nil {
			return err
		}
		if a.Engine.Governance().Effective("fixture").Weight != 7 || a.Engine.Governance().Effective("fixture").Request.MaxInflight != 3 {
			return fmt.Errorf("conflict merge lost unrelated field")
		}
		fake.SetRow(map[string]driver.Value{"cfg_key": "runtime_policy", "revision": int64(1), "document": []byte(`{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`)})
		if err = eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='策略历史').click()`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='查看差异')`)); err != nil {
			return err
		}
		if err = eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='查看差异').click()`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>document.querySelector('.rt-drawer input[type=checkbox]')!==null`)); err != nil {
			return err
		}
		if err = eval(`()=>{let inputs=Array.from(document.querySelectorAll('.rt-drawer input[type=checkbox]'));if(inputs.some(x=>x.checked))throw Error('history restore preselected');let c=inputs.find(x=>x.getAttribute('aria-label').includes('max_inflight'));c.checked=true;c.dispatchEvent(new Event('change'));Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='恢复所选字段并生成新版本').click()}`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>!document.querySelector('.rt-drawer')`)); err != nil {
			return err
		}
		if a.runtimeState.revision != 4 || a.Engine.Governance().Effective("fixture").Weight != 7 || a.Engine.Governance().Effective("fixture").Request.MaxInflight != 2 {
			return fmt.Errorf("history selected restore lost current settings")
		}
		if err = eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='资源控制').click();}`); err != nil {
			return err
		}
		if dir := os.Getenv("PAPA_RUNTIME_SCREENSHOT_DIR"); dir != "" {
			if err = os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			png, e := page.Screenshot(false, &proto.PageCaptureScreenshot{})
			if e != nil {
				return e
			}
			if err = os.WriteFile(filepath.Join(dir, "runtime-control.png"), png, 0600); err != nil {
				return err
			}
		}
		if err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: 390, Height: 844, DeviceScaleFactor: 1}); err != nil {
			return err
		}
		if err = eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='调整全局资源策略').click();let d=document.querySelector('.rt-drawer');let r=d.getBoundingClientRect();if(r.left<0||r.right>innerWidth+1)throw new Error('narrow drawer overflow');let n=Array.from(d.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled)'));n[n.length-1].focus();document.dispatchEvent(new KeyboardEvent('keydown',{key:'Tab',bubbles:true,cancelable:true}));if(document.activeElement!==n[0])throw new Error('focus escaped dialog');document.dispatchEvent(new KeyboardEvent('keydown',{key:'Tab',shiftKey:true,bubbles:true,cancelable:true}));if(document.activeElement!==n[n.length-1])throw new Error('backward focus escaped');document.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true,cancelable:true}));if(document.querySelector('.rt-drawer'))throw new Error('escape did not close');active.destroy();document.getElementById('root').replaceChildren();active=pages['account-pools'](document.getElementById('root'));}`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>document.querySelector('.rt-table')!==null`)); err != nil {
			return err
		}
		return eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='创建池').click();if(!document.querySelector('.rt-drawer'))throw new Error('account editor missing');active.destroy();if(document.querySelector('.rt-drawer'))throw new Error('destroy retained dialog');}`)
	})
	if err != nil {
		t.Fatal(err)
	}
}
