package diagnostics

import (
	"context"
	"fmt"
	"github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/internal/browserdiagnostics"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestIdentityUIRealBrowserDraftConflictBindingAndNavigation(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	a := identityTestHandler(t)
	p := diagnosticTestProfile()
	if err := a.Service.(*browserdiagnostics.Service).Manager().Store().AppendProfile(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/browser-identities", a.Identities)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><meta charset=utf-8><style>body{font:14px system-ui;margin:24px}.btn{padding:8px;border:1px solid #111;background:#ffd83d;margin:3px}input,select{padding:8px}button{cursor:pointer}</style><div id=root></div><script>window.pages={};window.Papa={page:(n,f)=>pages[n]=f};window.apiRequest=(u,o)=>fetch(u,o);window.copyText=()=>{};</script><script>`+runtimecontrol.Script+Script+`</script><script>window.active=pages['browser-diagnostics'](document.getElementById('root'));</script></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true, NoSandbox: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}, AllowedOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error {
		return b.WithNavigatedPage(ctx, srv.URL, browser.PageOptions{}, func(page *rod.Page) error {
			eval := func(js string) error { _, err := page.Eval(js); return err }
			wait := func(js string) error { return page.Timeout(8 * time.Second).Wait(rod.Eval(js)) }
			if err := wait(`()=>Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='追加版本')`); err != nil {
				return err
			}
			if err := eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='追加版本').click();const x=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent==='时区').querySelector('input');x.value='Asia/Tokyo';x.dispatchEvent(new Event('input'));Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='刷新').click()}`); err != nil {
				return err
			}
			p.Version = 2
			if err := a.Service.(*browserdiagnostics.Service).Manager().Store().AppendProfile(ctx, p, 1); err != nil {
				return err
			}
			if err := eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='保存新版本').click()`); err != nil {
				return err
			}
			if err := wait(`()=>document.querySelector('.rt-drawer').textContent.includes('草稿已保留')`); err != nil {
				return err
			}
			if err := eval(`()=>{const x=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent==='时区').querySelector('input');if(x.value!=='Asia/Tokyo')throw Error('draft lost');Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='核对最新版本').click()}`); err != nil {
				return err
			}
			if err := wait(`()=>document.querySelector('.rt-drawer pre')!==null`); err != nil {
				return err
			}
			if err := eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='采用已核对的最新版本号').click();Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='保存新版本').click()}`); err != nil {
				return err
			}
			if err := wait(`()=>!document.querySelector('.rt-drawer')`); err != nil {
				return err
			}
			if err := eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='设置身份绑定').click()`); err != nil {
				return err
			}
			if err := eval(`()=>{const d=document.querySelector('.rt-drawer');const inputs=d.querySelectorAll('input');inputs[0].value='fixture';inputs[0].dispatchEvent(new Event('input'));const kind=d.querySelector('select');kind.value='session';kind.dispatchEvent(new Event('change'));inputs[1].value='stable-business-session';inputs[1].dispatchEvent(new Event('input'));Array.from(d.querySelectorAll('button')).find(b=>b.textContent==='保存为待生效').click()}`); err != nil {
				return err
			}
			if err := wait(`()=>!document.querySelector('.rt-drawer')&&document.body.textContent.includes('stable-business-session')`); err != nil {
				return err
			}
			if err := eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='取消改绑').click()`); err != nil {
				return err
			}
			if err := wait(`()=>!Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='取消改绑')`); err != nil {
				return err
			}
			if dir := os.Getenv("PAPA_RUNTIME_SCREENSHOT_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					return err
				}
				png, err := page.Screenshot(false, &proto.PageCaptureScreenshot{})
				if err != nil {
					return err
				}
				if err = os.WriteFile(filepath.Join(dir, "browser-identity.png"), png, 0600); err != nil {
					return err
				}
			}
			if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: 390, Height: 844, DeviceScaleFactor: 1}); err != nil {
				return err
			}
			return eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='创建档案').click();const d=document.querySelector('.rt-drawer'),r=d.getBoundingClientRect();if(r.left<0||r.right>innerWidth+1)throw Error('narrow layout overflow');const nodes=Array.from(d.querySelectorAll('button:not(:disabled),input:not(:disabled),select:not(:disabled),textarea:not(:disabled)'));nodes[nodes.length-1].focus();document.dispatchEvent(new KeyboardEvent('keydown',{key:'Tab',bubbles:true,cancelable:true}));if(document.activeElement!==nodes[0])throw Error('focus escaped');const x=d.querySelector('input');x.value='changed';x.dispatchEvent(new Event('input'));window.confirm=()=>false;if(active.canLeave()!==false)throw Error('unguarded draft');active.destroy();if(document.querySelector('.rt-drawer'))throw Error('drawer leaked')}`)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}
