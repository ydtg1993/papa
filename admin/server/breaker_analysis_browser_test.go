package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	read := func(name string) string {
		b, err := staticFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	css := read("static/mo.css")
	controllers := read("features/shell/static/runtime.js") + read("features/shell/static/ui.js") + read("features/breakers/static/controller.js")
	cfg := MonitorConfig{VerifyToken: allowUnrestrictedTestRequests}
	now := time.Now()
	cfg.BreakerStatuses = func() map[string]core.BreakerStatus {
		return map[string]core.BreakerStatus{"fixture": {Site: "fixture"}}
	}
	cfg.BreakerReports = func(context.Context, string) ([]incident.Lifecycle, error) {
		out := []incident.Lifecycle{}
		for _, code := range []string{"429", "403"} {
			c := incident.Capture{ID: code, Site: "fixture", At: now, From: now.Add(-time.Minute), Window: time.Minute, Threshold: 3, Failures: 3}
			for i := 0; i < 3; i++ {
				c.Samples = append(c.Samples, incident.Sample{At: now, Stage: "detail", Kind: "retryable", TaskID: int64(i + 1), Error: "HTTP " + code + " <img src=x onerror=alert(1)>", URL: "https://example.test/detail?token=hidden"})
			}
			out = append(out, incident.Lifecycle{Capture: c, State: "cooldown", Evidence: c.Samples,
				Checks:   []incident.StageCheck{{Stage: "detail", State: "passed", Round: 1, TaskSuccesses: 1, Verified: true}, {Stage: "list", State: "failed", Round: 1, Detail: "HTTP " + code + " <img src=x onerror=alert(1)>"}, {Stage: "read", State: "passed", Round: 1, Verification: "readonly", Coverage: "全部读取和解析 <img src=x onerror=alert(1)>", ProbeSuccesses: 2, Verified: true}, {Stage: "write", State: "passed", Round: 1, ProbeSuccesses: 1}},
				Timeline: []incident.Transition{{At: now, Action: "stage_probe_failed", Stage: "list", Round: 1, Failures: 3, Reason: "阶段试探失败，整站继续暂停", NextAttemptAt: now.Add(time.Minute)}}})
		}
		return out, nil
	}
	mux := http.NewServeMux()
	NewMonitor(fakeStageStats, testLogger{t}, cfg).Register(mux)
	mux.HandleFunc("/fixture", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="zh"><meta charset="utf-8"><style>`+string(css)+`</style><body><button id="open" onclick="openBreakerAnalysis('fixture')">熔断分析</button><script>function esc(s){let n=document.createElement('span');n.textContent=s;return n.innerHTML;}function captureMonitorView(){return ()=>true;}function apiFetch(url){return fetch(url);}function copyText(s){window.copied=s;}async function downloadFile(url,current){let r=await fetch(url);if(current())window.downloaded=await r.text();}`+controllers+`</script></body></html>`)
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
		if _, err = page.Eval(`()=>{let b=document.querySelector('#breaker-analysis-body');if(!b.textContent.includes('全阶段验证进度')||!b.textContent.includes('验证通过')||!b.textContent.includes('验证失败')||!b.querySelector('ol li').textContent.includes('整站继续暂停'))throw new Error('missing operating timeline');}`); err != nil {
			return err
		}
		if _, err = page.Eval(`()=>{let b=document.querySelector('#breaker-analysis-body');for(let text of ['验证方式与覆盖范围','完整只读验证通过','全部读取和解析','未验证：仍需本轮真实任务成功'])if(!b.textContent.includes(text))throw new Error('missing verification: '+text);if(b.querySelector('img'))throw new Error('coverage HTML injection');}`); err != nil {
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
