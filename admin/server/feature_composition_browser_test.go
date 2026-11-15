package server

import (
	"context"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type browserExecutionPolicy struct{ ep.Service }

func (browserExecutionPolicy) Read(context.Context) (ep.View, error) {
	return ep.View{Document: ep.InitialDocument(), Revision: 1, AppliedRevision: 1, Status: "applied", Sites: []string{"synthetic"}}, nil
}

// Exercise the shipped HTML and historical script URLs together, including the
// default login gate, real oao dependency, feature navigation and identity reset.
func TestMonitorFeatureCompositionRealBrowser(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	mux := http.NewServeMux()
	NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{ExecutionPolicy: browserExecutionPolicy{}, VerifyToken: func(*http.Request) (string, bool) { return "synthetic-operator", true }, LogDir: t.TempDir()}).Register(mux)
	assets, err := new(oao.Oao).StaticFS()
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("/static/oao/", http.StripPrefix("/static/oao/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("/static/custom.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(TaskFlowScript))
	})
	mux.HandleFunc("/static/custom.css", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "text/css") })
	mux.HandleFunc("/api/oao/tables", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tables":[]}`))
	})
	mux.HandleFunc("/api/pages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pages":[]}`))
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tokens":[]}`))
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
		page, err := b.NewPage(ctx, srv.URL+"/monitor")
		if err != nil {
			return err
		}
		defer page.Close()
		if _, err = page.Eval(`()=>{if(!document.documentElement.classList.contains('need-login'))throw Error('login gate missing');if(!MonitorShell.controller('taskflow/editor'))throw Error('custom feature registration missing');window.featureErrors=[];addEventListener('error',e=>featureErrors.push(e.message));addEventListener('unhandledrejection',e=>featureErrors.push(String(e.reason)));document.getElementById('key').value='synthetic';submitKey();}`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>getMonitorSnapshot()!==null`)); err != nil {
			details, _ := page.Eval(`()=>JSON.stringify({errors:window.featureErrors,status:document.getElementById("statusText").textContent,hidden:document.hidden,poll:pollingStatus(),key:loadKey()})`)
			return fmt.Errorf("snapshot wait: %w; browser=%v", err, details)
		}
		for _, module := range []string{"logs", "obs", "tokens", "settings", "queues", "dashboard"} {
			if _, err = page.Eval(`name=>{selectNavigationSection(name==='settings'||name==='tokens'?'system':'monitor');if(!switchModule(name)||getActiveModule()!==name)throw Error('navigation failed: '+name);}`, module); err != nil {
				return err
			}

			if module == "settings" {
				if err = page.Timeout(5 * time.Second).Wait(rod.Eval(`()=>document.querySelectorAll('.ep-editor input').length===19`)); err != nil {
					details, _ := page.Eval(`()=>JSON.stringify({installed:!!MonitorShell.controller("executionpolicy/controller"),html:document.getElementById("custom-settings").innerHTML,inputs:document.querySelectorAll(".ep-editor input").length,form:document.querySelector(".ep-editor")?.textContent,errors:featureErrors})`)
					return fmt.Errorf("execution policy wait: %w; %v", err, details)
				}
				if _, err = page.Eval(`()=>{const e=document.querySelector('.ep-editor');if(!e.textContent.includes('保存版本 1')||!e.textContent.includes('高级恢复与证据参数'))throw Error('execution policy form incomplete');}`); err != nil {
					return err
				}
				if path := os.Getenv("PAPA_POLICY_SCREENSHOT"); path != "" {
					data, e := page.Screenshot(true, nil)
					if e != nil {
						return e
					}
					if e = os.WriteFile(path, data, 0600); e != nil {
						return e
					}
				}
			}
		}
		if _, err = page.Eval(`()=>{clearKey();if(getMonitorSnapshot()!==null||getSelectedSite()!==null||pollingStatus().enabled)throw Error('private session state survived');if(featureErrors.length)throw Error(featureErrors.join('\n'));if('obsState' in window||'logListSeq' in window||'monitorPoll' in window)throw Error('private state leaked globally');}`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
