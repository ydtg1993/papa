package app

import (
	"context"
	"fmt"
	featureaccounts "github.com/ydtg1993/papa/v4/admin/accounts"
	featurecontrol "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestAccountsUIRealBrowserDraftLoginIdentityAndSecretFields(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	a := accountsTestApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		a.accountsHandler().Accounts(w, r.WithContext(auth.WithOperator(r.Context(), "browser-operator")))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><div id="root"></div><script>window.pages={};window.Papa={page:(name,fn)=>pages[name]=fn};window.apiRequest=async function(url,o){return fetch(url,o)};</script><script>`+featurecontrol.Script+featureaccounts.Script+`</script><script>window.active=pages.accounts(document.getElementById('root'));</script></body></html>`)
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
		p, err := b.NewPage(ctx, srv.URL)
		if err != nil {
			return err
		}
		defer p.Close()
		wait := func(js string) error { return p.Timeout(5 * time.Second).Wait(rod.Eval(js)) }
		eval := func(js string) error { _, e := p.Eval(js); return e }
		if err = wait(`()=>document.querySelector('.rt-status').textContent.includes('先保存资料')`); err != nil {
			return err
		}
		if err = eval(`()=>{Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='新增账号').click();let fields=document.querySelectorAll('.ac-fields input');fields[0].value='browser-account';fields[1].value='浏览器合成账号';fields[2].value='alice';Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='刷新').click();}`); err != nil {
			return err
		}
		if err = eval(`()=>{if(document.querySelector('.ac-fields input').value!=='browser-account')throw Error('refresh erased draft');Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='保存资料').click()}`); err != nil {
			return err
		}
		if err = wait(`()=>Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='开始登录')`); err != nil {
			return err
		}
		if err = eval(`()=>{let field=document.querySelector('.ac-fields input');if(field.type!=='password')throw Error('secret input unmasked');field.value='private-password';Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='开始登录').click()}`); err != nil {
			return err
		}
		if err = wait(`()=>Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='身份正确，保存并启用')`); err != nil {
			return err
		}
		if _, err = a.Engine.GetAccountManager().Version(account.Key{Site: "fixture", ID: "browser-account"}); err == nil {
			return fmt.Errorf("UI automatically published unconfirmed identity")
		}
		if err = eval(`()=>{if(!document.querySelector('.ac-identity').textContent.includes('ali***'))throw Error('identity missing');if(document.body.textContent.includes('private-password'))throw Error('secret rendered');Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='身份正确，保存并启用').click()}`); err != nil {
			return err
		}
		if err = wait(`()=>document.body.textContent.includes('账号已保存并生效')&&document.querySelector('.rt-table tbody').textContent.includes('已核验')`); err != nil {
			return err
		}
		return eval(`()=>{if(sessionStorage.getItem('papa-account-login'))throw Error('finished workflow retained');if(document.body.textContent.includes('private-token'))throw Error('token leaked');window.active();}`)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The server deliberately holds old process/frame responses while the operator
// opens a different login. A real browser exercises DOM ownership and null
// field declarations, without relying on static string assertions for races.
func TestAccountsUIRealBrowserNullFieldsAndLateResponseOwnership(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	const mock = `window.pages={};window.Papa={page:(name,fn)=>pages[name]=fn};window.seq=0;window.ps={};window.reply=d=>new Response(JSON.stringify(d),{status:200,headers:{'Content-Type':'application/json'}});
window.apiRequest=async function(url,o){let b=o&&o.body?JSON.parse(o.body):null;
if(url==='/api/accounts'&&!b)return reply({configured:true,providers:[{site:'fixture',label:'Fixture',methods:[{id:'qr',kind:'qr',label:'扫码',fields:null},{id:'remote',kind:'remote',label:'页面登录',fields:null}]}],profiles:['one','two'].map(id=>({key:{site:'fixture',id},label:id,state:'draft',revision:1}))});
if(url==='/api/accounts'&&b.action==='start'){let id='p'+(++seq),p={id,key:b.key,revision:1,state:b.method==='qr'?'waiting_scan':'remote',expires_at:'2099-01-01T00:00:00Z'};ps[id]=p;return reply(p)}
if(url==='/api/accounts'&&b.action==='cancel'){ps[b.id]={...ps[b.id],state:'cancelled',revision:2};return reply(ps[b.id])}
if(url.startsWith('/api/accounts?process=')){let id=new URL(url,location.href).searchParams.get('process');if(id==='p2'&&!window.oldPollHeld){window.oldPollHeld=true;return new Promise(resolve=>window.oldPollResolve=()=>resolve(reply({...ps[id],revision:99,state:'confirm_identity',identity:{subject:'OLD',display:'OLD',scopes:[]}})))}if(id==='p3'&&window.holdClosed&&!window.closedHeld){window.closedHeld=true;return new Promise(resolve=>window.closedResolve=()=>{resolve(reply(ps[id]));setTimeout(()=>window.closedDelivered=true,50)})}return reply(ps[id])}
if(url==='/api/accounts/remote'){let frame={lease:'lease-'+b.id,width:1280,height:720,pages:[{id:'page',label:'Login'}],page:'page',image_data_url:'data:image/png;base64,'+(b.id==='p2'?'OLD_FRAME':'NEW_FRAME')};if(b.id==='p2'&&b.action==='claim')return new Promise(resolve=>window.oldFrameResolve=()=>resolve(reply(frame)));if(b.id==='p3')window.newFrameRequested=true;return reply(frame)}
throw Error('Unexpected mock request '+url)};`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><div id="root"></div><script>`+mock+`</script><script>`+featurecontrol.Script+featureaccounts.Script+`</script><script>window.active=pages.accounts(document.getElementById('root'));window.seenFrames=[];new MutationObserver(ms=>ms.forEach(m=>{if(m.attributeName==='src')seenFrames.push(m.target.src)})).observe(document.body,{subtree:true,attributes:true,attributeFilter:['src']});</script></body></html>`)
	}))
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
		wait := func(js string) error {
			if err := page.Timeout(6 * time.Second).Wait(rod.Eval(js)); err != nil {
				state, _ := page.Eval(`()=>JSON.stringify({seq:window.seq,oldFrame:!!window.oldFrameResolve,oldPoll:!!window.oldPollResolve,stored:sessionStorage.getItem("papa-account-login")})`)
				return fmt.Errorf("wait %s: %w state=%s", js, err, state.Value.Str())
			}
			return nil
		}
		eval := func(js string) error { _, e := page.Eval(js); return e }
		if err = wait(`()=>document.querySelectorAll('.rt-table tbody tr').length===2`); err != nil {
			return err
		}
		if err = eval(`()=>{document.querySelector('.rt-table tbody tr button').click();Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='开始登录').click()}`); err != nil {
			return err
		}
		if err = wait(`()=>Array.from(document.querySelectorAll('button')).some(b=>b.textContent==='检查扫码结果')`); err != nil {
			return err
		}
		if err = eval(`()=>Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='取消登录').click()`); err != nil {
			return err
		}
		if err = wait(`()=>document.body.textContent.includes('登录已取消')`); err != nil {
			return err
		}
		if err = eval(`()=>{document.querySelector('.rt-drawer button').click();document.querySelector('.rt-table tbody tr button').click();document.querySelector('.rt-drawer select').value='remote';document.querySelector('.rt-drawer select').dispatchEvent(new Event('change'));Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='开始登录').click()}`); err != nil {
			return err
		}
		if err = wait(`()=>!!window.oldFrameResolve&&!!window.oldPollResolve`); err != nil {
			return err
		}
		if err = eval(`()=>{document.querySelector('.rt-drawer button').click();document.querySelectorAll('.rt-table tbody tr')[1].querySelector('button').click();document.querySelector('.rt-drawer select').value='remote';document.querySelector('.rt-drawer select').dispatchEvent(new Event('change'));Array.from(document.querySelectorAll('button')).find(b=>b.textContent==='开始登录').click()}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('.rt-drawer').dataset.process==='p3'`); err != nil {
			return err
		}
		if err = eval(`()=>{oldFrameResolve();oldPollResolve()}`); err != nil {
			return err
		}
		if err = wait(`()=>window.newFrameRequested&&document.querySelector('.ac-remote').src.endsWith('NEW_FRAME')`); err != nil {
			return err
		}
		if err = eval(`()=>{if(seenFrames.some(x=>x.endsWith('OLD_FRAME')))throw Error('old frame crossed process');if(document.querySelector('.rt-drawer').dataset.process!=='p3'||document.querySelector('.rt-drawer').textContent.includes('OLD'))throw Error('old progress crossed process');window.holdClosed=true}`); err != nil {
			return err
		}
		if err = wait(`()=>!!window.closedResolve`); err != nil {
			return err
		}
		if err = eval(`()=>{document.querySelector('.rt-drawer button').click();closedResolve()}`); err != nil {
			return err
		}
		if err = wait(`()=>window.closedDelivered`); err != nil {
			return err
		}
		return eval(`()=>{if(document.querySelector('.rt-drawer'))throw Error('late poll reopened closed panel');window.active()}`)
	})
	if err != nil {
		t.Fatal(err)
	}
}
