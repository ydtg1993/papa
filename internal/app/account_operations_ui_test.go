package app

import (
	"context"
	"encoding/json"
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
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// Browser test exercises real pool policy persistence and the compensation
// interaction contract. Engine SQL and compensation CAS have separate tests.
func TestAccountOperationsUIRealBrowserPolicyAndPartialCompensation(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	a, _ := runtimeTestApp(t)
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	key := account.Key{Site: "fixture", ID: "primary"}
	if err := m.Register(key, account.Config{Origins: []string{"https://fixture.invalid"}, Login: func(context.Context, *account.Credentials) error { return nil }, Check: func(context.Context, *account.Credentials) (bool, error) { return true, nil }}); err != nil {
		t.Fatal(err)
	}
	a.Engine.SetAccountManager(m)
	pools, err := account.NewPools(context.Background(), m, account.NewMemoryPoolStore(), account.PoolOptions{ScanInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	a.runtimeState.pools = pools
	if err := a.runtimeState.service.AttachPools(pools); err != nil {
		t.Fatal(err)
	}
	_, err = pools.Upsert(context.Background(), account.PoolConfig{Key: account.PoolKey{Site: "fixture", Name: "main"}, Members: []string{"primary"}, Disabled: true, StandbyTarget: 1, StandbyTTL: time.Minute}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/account-pools", a.accountPoolsAPI)
	mux.HandleFunc("/api/account-compensation", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeRuntimeJSON(w, 200, map[string]any{"eligible": 2, "blocked": 1, "history_limit": 8, "items": []map[string]any{
				{"id": 11, "stage": "detail", "mode": "", "version": 7, "eligible": true},
				{"id": 12, "stage": "detail", "mode": "", "version": 8, "eligible": true},
				{"id": 13, "stage": "detail", "mode": "permanent", "version": 9, "eligible": false, "blocked_reason": "permanently terminated"},
			}})
			return
		}
		var body struct {
			Action   string `json:"action"`
			Requests []struct {
				ID      uint   `json:"id"`
				Version uint64 `json:"version"`
			} `json:"requests"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Action != "retry" || len(body.Requests) != 2 || body.Requests[0].ID != 11 || body.Requests[0].Version != 7 || body.Requests[1].ID != 12 {
			http.Error(w, "invalid snapshot", 400)
			return
		}
		writes.Add(1)
		writeRuntimeJSON(w, 200, map[string]any{"results": []map[string]any{{"id": 11, "submitted": true}, {"id": 12, "submitted": false, "error": "版本已变化"}}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><meta charset="utf-8"><div id="root"></div><script>window.pages={};window.Papa={page:(n,f)=>pages[n]=f};window.apiRequest=(u,o)=>fetch(u,o);window.confirm=()=>true;window.openPage=()=>{};</script><script>`+runtimeControlScript+accountPoolsScript+`</script><script>window.active=pages['account-pools'](document.getElementById('root'));</script></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true, NoSandbox: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	err = pool.WithBrowser(ctx, func(b *browser.Browser) error {
		page, err := b.NewPage(ctx, srv.URL)
		if err != nil {
			return err
		}
		defer page.Close()
		eval := func(js string) error { _, e := page.Eval(js); return e }
		wait := func(js string) error {
			if err := page.Timeout(8 * time.Second).Wait(rod.Eval(js)); err != nil {
				body, _ := page.Eval(`()=>document.body.innerText`)
				return fmt.Errorf("wait %s: %w; fixture page: %v", js, err, body)
			}
			return nil
		}
		if err = wait(`()=>document.querySelector('.rt-table tbody tr')!==null`); err != nil {
			return err
		}
		if err = eval(`()=>{window.click=(text)=>{let b=Array.from(document.querySelectorAll('button')).find(x=>x.textContent===text);if(!b)throw Error('missing '+text);b.click()};click('设置');click('保存')}`); err != nil {
			return err
		}
		if err = wait(`()=>!document.querySelector('.rt-drawer')`); err != nil {
			return err
		}
		snapshot, e := pools.Get(account.PoolKey{Site: "fixture", Name: "main"})
		if e != nil || snapshot.StandbyTarget != 1 || snapshot.StandbyTTL != time.Minute {
			return fmt.Errorf("pool editor lost standby settings: %v", e)
		}
		if err = eval(`()=>{click('查看');click('健康与使用策略');let row=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent.startsWith('每日操作上限'));let input=row.querySelector('input');input.value='25';input.dispatchEvent(new Event('input'));click('刷新')}`); err != nil {
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
			if err := os.WriteFile(filepath.Join(dir, "account-policy.png"), png, 0600); err != nil {
				return err
			}
		}
		if err = eval(`()=>{let row=Array.from(document.querySelectorAll('.rt-field')).find(n=>n.textContent.startsWith('每日操作上限'));if(row.querySelector('input').value!=='25')throw Error('refresh overwrote policy draft');click('保存策略')}`); err != nil {
			return err
		}
		if err = wait(`()=>!document.querySelector('.rt-drawer')`); err != nil {
			return err
		}
		policy, _, e := pools.Policy(key)
		if e != nil || policy.DailyLimit != 25 {
			return fmt.Errorf("policy not persisted: %v", e)
		}
		if err = eval(`()=>{click('查看');click('关联任务')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelectorAll('.rt-drawer input[type=checkbox]').length===3`); err != nil {
			return err
		}
		if err = eval(`()=>{let c=Array.from(document.querySelectorAll('.rt-drawer input[type=checkbox]'));if(!c[2].disabled)throw Error('permanent task selectable');c.slice(0,2).forEach(x=>{x.checked=true;x.dispatchEvent(new Event('change'))});click('补偿所选任务')}`); err != nil {
			return err
		}
		if err = wait(`()=>document.querySelector('.rt-drawer').textContent.includes('版本已变化')`); err != nil {
			return err
		}
		if err = eval(`()=>{if(!document.querySelector('.rt-drawer').textContent.includes('已提交'))throw Error('missing partial success');let b=Array.from(document.querySelectorAll('button')).find(x=>x.textContent==='补偿所选任务');if(!b.disabled)throw Error('stale resubmit enabled');click('重新预览')}`); err != nil {
			return err
		}
		if err = wait(`()=>Array.from(document.querySelectorAll('.rt-drawer input[type=checkbox]')).filter(x=>!x.disabled).length===2`); err != nil {
			return err
		}
		return eval(`()=>{active.destroy();if(document.querySelector('.rt-drawer'))throw Error('dialog retained')}`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 {
		t.Fatal("mutation repeated", writes.Load())
	}
}
