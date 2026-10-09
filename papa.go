// Package papa 是一个可复用的爬虫框架。
// 在 main.go 中 import 本包，用 New 创建应用、RegisterSites 注册站点与阶段即可运行。
//
// 本文件是**应用门面**：App 及其初始化选项、后台扩展点（表格页 / 自定义页 / 路由）。
// 任务与引擎见 task.go，错误与告警见 errors.go。
package papa

import (
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/core"
	"github.com/ydtg1993/papa/v2/internal/app"
)

// App 应用容器，封装配置、日志、数据库、爬虫引擎。
type App = app.App

// Option 应用初始化选项，传给 New。
type Option = app.Option

// Router 业务后台路由表，在 App.UseRouter 的回调里声明路径、方法与中间件。
type Router = app.Router

// Middleware 一个 HTTP 中间件：func(http.Handler) http.Handler。
// 业务中间件与后台鉴权（框架注入的那个）是同一个类型，可以串成一条链。
type Middleware = app.Middleware

// Page 一个自定义后台页，传给 App.UsePage。
type Page = app.Page

// SiteSpec 站点声明（Key / BaseURL / 熔断阈值 / 阶段清单），传给 App.RegisterSites。
type SiteSpec = app.SiteSpec

// StageSpec 一个阶段的完整声明：fetcher + 并发/队列/间隔/重试/入口开关。
type StageSpec = app.StageSpec

// RetrySpec 重试声明（MaxAttempts / Backoff）。
type RetrySpec = app.RetrySpec

// BreakerSpec 站点自己的熔断阈值（覆盖 crawler.breaker 的默认值）。
type BreakerSpec = app.BreakerSpec

// Site 站点声明的快照（engine.Site(key) 返回它：BaseURL 等）。
type Site = core.Site

// Config 全局配置。
type Config = config.Config

// RegisterSite 登记一个站点声明（**在站点文件的 init() 里调**）：
//
//	// configs/sites/huangguo.go
//	func init() { papa.RegisterSite(huangguo()) }
//
// 框架自己收集，项目不用手写汇总清单 —— 加一个站 = 加一个文件。
// main.go 只需一行匿名导入（`_ "yourmod/configs/sites"`）让这些 init 跑起来，
// 再把收集到的交给 App：`app.RegisterSites(papa.Sites()...)`。
var RegisterSite = app.RegisterSite

// Sites 返回框架收集到的全部站点声明（顺序 = 文件名字典序）。传给 `App.RegisterSites`。
var Sites = app.Sites

// WithHeaders 给这次抓取（及其派生 ctx 上的后续抓取）带上/覆盖请求头，静态与浏览器两条路都认：
//
//	doc, finalURL, err := engine.FetchRendered(papa.WithHeaders(ctx, map[string]string{
//	    "Referer": "https://example.com/list",
//	}), url, ".detail")
//
// 同键覆盖，值空串 = 删掉那个头；它是**叠加**的 —— 站点级（SiteSpec.Headers）已挂在 ctx 上时，
// 这里只覆盖你给的键。优先级：框架默认 < 全局 headers 配置 < 站点 headers < 逐请求。
var WithHeaders = core.WithHeaders

// WithProxyURL 指定这次抓取走**哪个**代理（如 "http://1.2.3.4:8080"）；空串 = 不指定。
//
//	page, err := engine.FetchHTML(papa.WithProxyURL(ctx, engine.NextProxy()), url)
//
// 比 `htmlfetch.WithProxy(ctx, use)` 更具体，优先于它；**浏览器路径不支持**逐请求代理地址
// （代理是浏览器实例级设置），传了会明确报错而不是静默忽略。
var WithProxyURL = core.WithProxyURL

// ResolveURL 把页面上的相对地址按 base 解析成绝对地址：只收 http/https（`javascript:`、`mailto:` 进不了任务队列）、
// 去掉 fragment（`/a#x` 与 `/a#y` 是同一页）、解析不出 host 就报错。
//
//	u, err := papa.ResolveURL(page.URL.String(), sel.AttrOr("href", ""))
//	if err != nil { continue }                       // 残缺/非 http 链接跳过
//	if !papa.SameHost(site.BaseURL, u) { continue }   // 只跟本站的链接
var ResolveURL = core.ResolveURL

// SameHost 判断两个地址（URL 或裸 host）是不是同一个 host：忽略大小写、端口、末尾的点，
// 并把 `www.` 前缀视作同一个 host（apex 与 www 在爬虫里几乎总是同一个站）。
var SameHost = core.SameHost

// IsSubdomainOf 判断 child 是不是 parent 的**子域**，按点边界比对 ——
// `evil-example.com` 不是 `example.com` 的子域。CDN 那类资源域名用它：
// `SameHost(base, u) || IsSubdomainOf(u, base)`。
var IsSubdomainOf = core.IsSubdomainOf

// WithConfigPath 指定配置文件路径（缺省读 PAPA_CONFIG 环境变量，再回退 configs/config.yaml）
var WithConfigPath = app.WithConfigPath

// WithModels 追加需要建表的业务模型；框架自带的表不用登记。
// 它们会在 App.Migrate()（脚手架里的 `make migrate`）时一起建。
var WithModels = app.WithModels

// New 创建应用实例，完成配置加载、日志、数据库、引擎的初始化。
func New(opts ...Option) (*App, error) {
	return app.NewApp(opts...)
}
