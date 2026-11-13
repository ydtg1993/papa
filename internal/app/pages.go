package app

import (
	"fmt"
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/server"
	"net/http"
	"os"
	"strings"
)

// UseTables 注册监控后台的表格页（须在 Run 之前调用 —— 路由在 Run 时挂载）。
// 表格声明与数据来源见组件库 github.com/ydtg1993/oao：业务声明"显示什么、
// 怎么显示"并实现 oao.Source 提供数据，框架不自动扫描业务表。
// Key 不得与 task/operation_log 重复。
// 放在 New 之后是为了能用 app.DB 构造 Source。
func (a *App) UseTables(tables ...oao.Table) {
	a.tables = append(a.tables, tables...)
}

// UseRouter 注册一组挂在后台服务上的业务路由（须在 Run 之前调用 —— 路由在 Run 时挂载）。
//
// 回调里用 Router 声明路径、方法与中间件，handler 就是一个普通的 http.HandlerFunc：
//
//	app.UseRouter(func(r *papa.Router) {
//	    r.Use(myLogging)                        // 业务中间件，注册顺序正序执行
//	    r.Group("/api/review", func(g *papa.Router) {
//	        g.Get("/list", listHandler)         // GET /api/review/list
//	        g.Post("/approve", approveHandler)  // POST /api/review/approve
//	    })
//	})
//
// 这些路由默认和后台内置接口一样过「IP 白名单 + 访问令牌」；必须对外公开的（webhook / OAuth 回调）
// 用 r.NoAuth() 显式声明。路由在 HTTP 服务（server.enabled）开启时才挂载 —— 关掉时不会静默：
// 启动会打一条醒目错误日志说明它们没生效。
//
// fn 为 nil 直接 panic：与 UsePage / RegisterSites 同风格，声明有问题启动即失败。
func (a *App) UseRouter(fn func(*Router)) {
	if fn == nil {
		panic(fmt.Errorf("UseRouter: 回调为 nil —— 里面要用 Router 声明路由"))
	}
	a.routers = append(a.routers, fn)
}

// Page 一个自定义后台页：清单进入所属分类的导航，渲染由 Script 提供。
// Script 是**受信任代码**，在里面调 Papa.page(Key, fn) 注册渲染函数：
//
//	app.UsePage(papa.Page{
//	    Key: "review", Label: "审核", Group: "业务",
//	    Script: `Papa.page("review", function (el, meta) {
//	        el.innerHTML = "<h2>" + esc(meta.label) + "</h2>";   // Toast/Dialog/apiFetch 等都是全局可用的
//	    });`,
//	})
type Page struct {
	Owns    []string // 可选归属入口，例如 module:rules / page:operations；仅改变菜单，不改变权限。
	Key     string   // 菜单与 DOM 标识：字母、数字、下划线、连字符
	Label   string   // 菜单文案，留空用 Key
	Group   string   // 分类内的业务分组，留空归 "General"
	Section string   // 可选导航分类：flow / monitor / data / system；留空或未知值归 data
	Script  string   // 受信任 JS，必须调 Papa.page(Key, fn) 注册渲染
}

// UsePage 注册一个自定义页（须在 Run 之前调用 —— 路由在 Run 时挂载）。
// Key 非法、重复或 Script 为空直接 panic；Section 留空或未知值默认归入 data。
func (a *App) UsePage(p Page) {
	if !validPageKey(p.Key) {
		panic(fmt.Errorf("invalid page key %q: 只允许字母、数字、下划线、连字符", p.Key))
	}
	for _, x := range a.pages {
		if x.Key == p.Key {
			panic(fmt.Errorf("duplicate page key %q", p.Key))
		}
	}
	if strings.TrimSpace(p.Script) == "" {
		panic(fmt.Errorf("page %q: Script 为空 —— 里面要调 Papa.page(%q, fn) 注册渲染", p.Key, p.Key))
	}
	validatePageOwnership(a.pages, p)
	p.Owns = append([]string(nil), p.Owns...)
	a.pages = append(a.pages, p)
	a.extraJS = append(a.extraJS, p.Script)
}

// UseScript 往后台页注入一段受信任 JS（拼进 /static/custom.js）。
// 它能用的全局件见 docs/operations/MONITOR.md：Toast / Dialog / skeletonRows / esc / apiFetch / apiPost 等。
//
// 注意：注入内容与 mo.js / oao.js 一样走**免鉴权**静态路由（浏览器标签没法带自定义头），
// 所以别把密钥、令牌之类写进去 —— 数据接口仍在白名单 + 密钥后面。
func (a *App) UseScript(js string) {
	if strings.TrimSpace(js) == "" {
		return
	}
	a.extraJS = append(a.extraJS, js)
}

// UseScriptFile 读一个 .js 文件注入；读不到直接 panic（部署问题，不静默）。
func (a *App) UseScriptFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Errorf("UseScriptFile %s: %w", path, err))
	}
	a.UseScript(string(b))
}

// UseCSS 注入一段样式（拼进 /static/custom.css）。它在 mo.css / oao.css 之后加载，可以覆盖。
func (a *App) UseCSS(css string) {
	if strings.TrimSpace(css) == "" {
		return
	}
	a.extraCSS = append(a.extraCSS, css)
}

// UseCSSFile 读一个 .css 文件注入；读不到直接 panic。
func (a *App) UseCSSFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Errorf("UseCSSFile %s: %w", path, err))
	}
	a.UseCSS(string(b))
}

// validPageKey 页面 key 会进 DOM 与菜单标识，限制成 URL/DOM 安全字符（与 oao 的表 key 同一套规则）。
func validPageKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// mountCustomRoutes 挂自定义页与注入的三条路由（在监控后台的装配里调用）。
//
//   - 清单走 /api/pages，和白名单 + 密钥一起校验（菜单是数据）；
//   - 脚本/样式走 /static/，与 oao.js 同一条**免鉴权**静态路由 —— 浏览器 <script src>/<link>
//     没法带自定义头，而注入内容按"非机密"对待（见 UseScript 注释）。
//
// 三条路由**无条件注册**（没有内容时返回空体 / 空清单），这样 template.html 里的两个标签是静态的，
// 宿主没注入东西也不会 404。抽成独立方法是为了能在没有数据库时单测（完整装配需要 MySQL）。
func (a *App) mountCustomRoutes(mux *http.ServeMux, wrap Middleware) {
	pages := make([]server.ExtensionPage, 0, len(a.pages))
	for _, p := range a.pages {
		pages = append(pages, server.ExtensionPage{Key: p.Key, Label: p.Label, Group: p.Group, Section: p.Section, Owns: append([]string(nil), p.Owns...)})
	}
	server.RegisterExtensions(mux, wrap, pages, a.extraJS, a.extraCSS)
}

// mountRouters 把业务用 UseRouter 注册的路由组挂到后台 mux 上（在监控后台的装配里调用）。
// guard 是后台那套「白名单 + 令牌」中间件，Router 默认给每条业务路由套上，NoAuth() 的组除外。
// 与 mountCustomRoutes 一样抽成独立方法，好在没有数据库时单测。
func (a *App) mountRouters(mux *http.ServeMux, guard Middleware) {
	for _, fn := range a.routers {
		fn(newRouter(mux, guard))
	}
}
