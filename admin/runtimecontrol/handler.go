package runtimecontrol

import (
	"context"
	_ "embed"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/internal/httpjson"
	"github.com/ydtg1993/papa/v4/core"
	domain "github.com/ydtg1993/papa/v4/internal/runtimecontrol"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/middleware/proxy"
	"net/http"
)

//go:embed static/runtime_control.js
var Script string

type PolicyService interface {
	Read(context.Context) Result
	Save(context.Context, Update, string) Result
	Acknowledge(string, uint64) Result
	History(context.Context) Result
	HistoryAction(context.Context, HistoryRequest, string) Result
}
type Runtime interface {
	GetSiteStats() map[string]core.SiteStat
	GetStageStats() map[string]core.StageStats
	Governance() *governance.Controller
	RuntimeDeclarations() map[string]map[string]any
	GetBrowserPool() *browser.Pool
	GetProxy() *proxy.Manager
	Site(string) (core.Site, bool)
	RuntimeContext(context.Context, string) context.Context
}
type Handler struct {
	Service PolicyService
	Runtime Runtime
	Pools   *account.Pools
	Network egress.Config
	Audit   func(*http.Request, string, string, string, error)
}

func (a *Handler) runtimeAudit(r *http.Request, t, action, id string, err error) {
	if a.Audit != nil {
		a.Audit(r, t, action, id, err)
	}
}
func (a *Handler) enrich(v map[string]any) {
	v["sites"] = a.Runtime.GetSiteStats()
	v["stages"] = a.Runtime.GetStageStats()
	v["coverage"] = []string{"框架 HTTP、账号 HTTP、文件和 M3U8 请求（须传递任务 context；下载认证参数由业务提供）", "受管浏览器页面：首个文档同源及显式 GovernanceOrigins 的导航、XHR、子资源和重定向", "弹窗、独立 Worker、原始 Rod 页面和覆盖 CDP Fetch 处理器不在受控范围", "业务自行创建且不使用框架出口的客户端不受控"}
	v["declarations"] = a.Runtime.RuntimeDeclarations()
	if p := a.Runtime.GetBrowserPool(); p != nil {
		v["public_browser_pool"] = p.Stats()
	}
}
func (a *Handler) respond(w http.ResponseWriter, r *http.Request, out Result) {
	for _, e := range out.Events {
		a.runtimeAudit(r, "runtime_policy", e.Action, e.ID, e.Err)
	}
	if _, ok := out.Value["document"]; ok {
		a.enrich(out.Value)
	}
	if latest, ok := out.Value["latest"].(map[string]any); ok {
		a.enrich(latest)
	}
	code := http.StatusOK
	switch out.Kind {
	case domain.Invalid:
		code = 400
	case domain.Conflict:
		code = 409
	case domain.Unavailable:
		code = 503
	case domain.NotFound:
		code = 404
	}
	httpjson.Write(w, code, out.Value)
}
func (a *Handler) Policy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.respond(w, r, a.Service.Read(r.Context()))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input Update
	if err := httpjson.Strict(http.MaxBytesReader(w, r.Body, 128<<10), &input); err != nil {
		httpjson.Write(w, 400, map[string]any{"error": "运行策略 JSON 无效: " + err.Error()})
		return
	}
	a.respond(w, r, a.Service.Save(r.Context(), input, auth.OperatorFrom(r.Context())))
}
func (a *Handler) Remote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input struct {
		Site     string `json:"site"`
		Revision uint64 `json:"revision"`
	}
	if err := httpjson.Strict(http.MaxBytesReader(w, r.Body, 4096), &input); err != nil {
		httpjson.Write(w, 400, map[string]any{"error": "无效请求"})
		return
	}
	a.respond(w, r, a.Service.Acknowledge(input.Site, input.Revision))
}
func (a *Handler) History(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.respond(w, r, a.Service.History(r.Context()))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input HistoryRequest
	if err := httpjson.Strict(http.MaxBytesReader(w, r.Body, 128<<10), &input); err != nil || (input.Action != "preview" && input.Action != "restore") {
		httpjson.Write(w, 400, map[string]any{"error": "历史策略请求无效"})
		return
	}
	a.respond(w, r, a.Service.HistoryAction(r.Context(), input, auth.OperatorFrom(r.Context())))
}
