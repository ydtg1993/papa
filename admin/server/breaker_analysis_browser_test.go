package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
)

func TestBreakerAnalysisRealBrowser(t *testing.T) {
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	raw, _ := staticFS.ReadFile("static/mo.js")
	css, _ := staticFS.ReadFile("static/mo.css")
	source := string(raw)
	dialog := source[strings.Index(source, "var Dialog = (function ()"):strings.Index(source, "/** skeleton 生成")]
	analysis := source[strings.Index(source, "var breakerAnalysisSeq = 0;"):strings.Index(source, "// 暂停抓取：")]
	m := newFakeMonitor(t)
	now := time.Now()
	m.cfg.BreakerStatuses = func() map[string]core.BreakerStatus {
		return map[string]core.BreakerStatus{"fixture": {Site: "fixture"}}
	}
	m.cfg.BreakerIncidents = func(string) []incident.Capture {
		out := []incident.Capture{}
		for _, code := range []string{"429", "403"} {
			c := incident.Capture{ID: code, Site: "fixture", At: now, From: now.Add(-time.Minute), Window: time.Minute, Threshold: 3, Failures: 3}
			for i := 0; i < 3; i++ {
				c.Samples = append(c.Samples, incident.Sample{At: now, Stage: "detail", Kind: "retryable", TaskID: int64(i + 1), Error: "HTTP " + code + " <img src=x onerror=alert(1)>", URL: "https://example.test/detail?token=hidden"})
			}
			out = append(out, c)
		}
		return out
	}
	mux := http.NewServeMux()
	m.Register(mux)
	mux.HandleFunc("/fixture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="zh"><meta charset="utf-8"><style>`+string(css)+`</style><body><button id="open" onclick="openBreakerAnalysis('fixture')">熔断分析</button><script>function esc(s){let n=document.createElement('span');n.textContent=s;return n.innerHTML;}function captureMonitorView(){return ()=>true;}function apiFetch(url){return fetch(url);}function copyText(s){window.copied=s;}async function downloadFile(url,current){let r=await fetch(url);if(current())window.downloaded=await r.text();}`+dialog+analysis+`</script></body></html>`)
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
		page, err := b.NewPage(ctx, srv.URL+"/fixture")
		if err != nil {
			return err
		}
		defer page.Close()
		if _, err = page.Eval(`()=>document.getElementById('open').click()`); err != nil {
			return err
		}
		if err = page.Timeout(8 * time.Second).Wait(rod.Eval(`()=>document.querySelector('#breaker-analysis-body pre')!==null`)); err != nil {
			return err
		}
		if _, err = page.Eval(`()=>{if(document.querySelector('#breaker-analysis-body img'))throw new Error('HTML injection');if(!document.querySelector('#breaker-analysis-body').textContent.includes('HTTP 429'))throw new Error('missing analysis');Array.from(document.querySelectorAll('.dlg-actions button')).find(b=>b.textContent==='复制报告').click();if(!window.copied.includes('HTTP 429')||window.copied.includes('hidden'))throw new Error('copy evidence');}`); err != nil {
			return err
		}
		for _, width := range []int{1100, 390} {
			if err = page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: width, Height: 900, DeviceScaleFactor: 1}); err != nil {
				return err
			}
			if _, err = page.Eval(`()=>{let d=document.querySelector('.dlg'),r=d.getBoundingClientRect();if(r.left<0||r.right>innerWidth+1||d.scrollWidth>d.clientWidth+1)throw new Error('dialog overflow');}`); err != nil {
				return err
			}
			if dir := os.Getenv("PAPA_BREAKER_SCREENSHOT_DIR"); dir != "" {
				if err = os.MkdirAll(dir, 0700); err != nil {
					return err
				}
				png, e := page.Screenshot(false, &proto.PageCaptureScreenshot{})
				if e != nil {
					return e
				}
				if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("breaker-%d.png", width)), png, 0600); err != nil {
					return err
				}
			}
		}
		_, err = page.Eval(`()=>{let s=document.getElementById('breaker-analysis-select');s.value='1';s.dispatchEvent(new Event('change'));if(!document.querySelector('#breaker-analysis-body pre').textContent.includes('HTTP 403'))throw new Error('history selection');document.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}));if(document.querySelector('.dlg-mask.open'))throw new Error('escape failed');}`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
