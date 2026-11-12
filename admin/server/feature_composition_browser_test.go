package server

import (
	"context"
	"fmt"
	"github.com/go-rod/rod"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// Exercise the shipped HTML and historical script URLs together, including the
// default login gate, real oao dependency, feature navigation and identity reset.
func TestMonitorFeatureCompositionRealBrowser(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	mux := http.NewServeMux()
	NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests, LogDir: t.TempDir()}).Register(mux)
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
			if _, err = page.Eval(`name=>{if(!switchModule(name)||getActiveModule()!==name)throw Error('navigation failed: '+name);}`, module); err != nil {
				return err
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
