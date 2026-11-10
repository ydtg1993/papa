package app

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

func (a *App) proxyHealthAPI(w http.ResponseWriter, r *http.Request) {
	m := a.Engine.GetProxy()
	if m == nil {
		writeRuntimeJSON(w, 503, map[string]any{"error": "未配置代理管理器"})
		return
	}
	if r.Method == http.MethodGet {
		writeRuntimeJSON(w, 200, m.Snapshot())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input struct {
		Action   string `json:"action"`
		ID       string `json:"id"`
		Revision uint64 `json:"revision"`
		Site     string `json:"site"`
	}
	if err := strictJSON(http.MaxBytesReader(w, r.Body, 4<<10), &input); err != nil {
		writeRuntimeJSON(w, 400, map[string]any{"error": "代理操作格式无效"})
		return
	}
	if !regexp.MustCompile(`^proxy_[a-f0-9]{32}$`).MatchString(input.ID) || (input.Action != "disable" && input.Action != "enable" && input.Action != "probe") {
		writeRuntimeJSON(w, 400, map[string]any{"error": "无效代理标识或动作"})
		return
	}
	var err error
	switch input.Action {
	case "disable":
		err = m.DisableCAS(input.ID, input.Revision)
	case "enable":
		err = m.EnableCAS(input.ID, input.Revision)
	case "probe":
		site, ok := a.Engine.Site(input.Site)
		if !ok || site.BaseURL == "" {
			err = errors.New("请选择有 BaseURL 的已注册站点")
			break
		}
		ctx, cancel := context.WithTimeout(a.Engine.RuntimeContext(r.Context(), input.Site), 15*time.Second)
		defer cancel()
		var ref *proxy.Reference
		ref, err = m.AcquireProbeCAS(ctx, input.ID, input.Revision)
		if err != nil {
			break
		}
		defer ref.Release()
		u, parseErr := url.Parse(ref.URL())
		if parseErr != nil {
			err = errors.New("代理地址无效")
			break
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = http.ProxyURL(u)
		client := egress.Client(&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, a.Config.Network)
		defer client.CloseIdleConnections()
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodHead, site.BaseURL, nil)
		if err == nil {
			var response *http.Response
			response, err = client.Do(req)
			code := 0
			if response != nil {
				code = response.StatusCode
				_ = response.Body.Close()
			}
			ref.ReportHTTP(code, err)
		}
	default:
		err = errors.New("未知代理操作")
	}
	safeErr := err
	if err != nil {
		safeErr = errors.New("代理操作未完成")
	}
	a.runtimeAudit(r, "proxy_health", input.Action, input.ID, safeErr)
	if err != nil {
		code := 400
		if errors.Is(err, proxy.ErrConflict) {
			code = 409
		}
		writeRuntimeJSON(w, code, map[string]any{"error": "代理操作未完成，请刷新状态后核对", "latest": m.Snapshot()})
		return
	}
	writeRuntimeJSON(w, 200, m.Snapshot())
}
