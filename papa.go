// Package papa 是一个可复用的爬虫框架。
// 在 main.go 中 import 本包，用 New 创建应用、RegisterSites 注册站点与阶段即可运行。
//
// 本文件是**应用门面**：App 及其初始化选项、后台扩展点（表格页 / 自定义页 / 路由）。
// 任务与引擎见 task.go，错误与告警见 errors.go。
package papa

import (
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/internal/app"
	"github.com/ydtg1993/papa/v3/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
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

// StageSpec 一个阶段的完整声明：fetcher + 并发/队列/间隔/重试/入口模式。
type StageSpec = app.StageSpec

// EntryMode 阶段入口的模式（`StageSpec.Entry`）：
//
//	EntryAuto   启动那一趟就投入口任务（`SubmitEntries`）
//	EntryManual 启动不投，等后台「阶段入口」卡片上点火（`Engine.SubmitStageEntries`）
//	EntryOff    零值/不写：启动不投、后台也不显示这一行
//
// 要手动点火就必须先声明成 `EntryManual` —— 后台只给 manual 的阶段按钮。
type EntryMode = engine.EntryMode

const (
	EntryOff    = engine.EntryOff
	EntryAuto   = engine.EntryAuto
	EntryManual = engine.EntryManual
)

// RetrySpec 重试声明（MaxAttempts / Backoff）。
type RetrySpec = app.RetrySpec

// BreakerSpec 站点自己的熔断阈值（覆盖 crawler.breaker 的默认值）。
type BreakerSpec = app.BreakerSpec

// M3U8Spec / FileDownSpec 下载器的**覆盖层**：站点声明（SiteSpec.M3U8 / FileDown）与阶段声明
// （StageSpec.M3U8 / FileDown）用它，**字段全是指针，只写要改的那几项**：
//
//	M3U8: &papa.M3U8Spec{OutputDir: &dir, Output: &papa.OutputSegments}
//
// 解析顺序：**阶段 > 站点 > 全局**（全局那份 = main.go 里 `SetM3U8` / `SetFiledown` 给的）。
// fetcher 里要"这一阶段该用的那个"就 `engine.M3U8ForTask(task)`；要临时一份只给这次调用用，
// 就 `.With(&papa.M3U8Spec{...})`（配置独立、内部状态共享）；要完全独立的一份，
// 直接 `m3u8.NewDownloader(&cfg)` —— 三条路都在，框架不拦。
type M3U8Spec = m3u8.Overlay

// DescribeDownloadOverlays 列出各站点/阶段改过的下载器配置项（`papa check` 用它说清"到底哪份生效"）。
func DescribeDownloadOverlays(sites ...SiteSpec) []string {
	return app.DescribeDownloadOverlays(sites...)
}

type FileDownSpec = filedown.Overlay

// OutputMode 产物形态（m3u8 下载器）：合并成一个文件 / 只留切片组 / 只存播放列表。
type OutputMode = m3u8.OutputMode

const (
	OutputMerge    = m3u8.OutputMerge    // 默认：ffmpeg remux 成一个文件（扩展名由 MergeExt 决定）
	OutputSegments = m3u8.OutputSegments // 不产出单文件，只留切片组（挪到 <OutputDir>/<file>/ 下）
	OutputPlaylist = m3u8.OutputPlaylist // 只把 .m3u8 原文存下来，一个分片都不下
)

// Site 站点声明的快照（engine.Site(key) 返回它：BaseURL 等）。
type Site = core.Site

// Config 全局配置。
type Config = config.Config

// RegisterSite 登记一个站点声明（**在站点文件的 init() 里调**）：
//
//	// sites/target_site.go
//	func init() { papa.RegisterSite(target_site()) }
//
// 框架自己收集，项目不用手写汇总清单 —— 加一个站 = 加一个文件。
// main.go 只需一行匿名导入（`_ "yourmod/sites"`）让这些 init 跑起来，
// 再把收集到的交给 App：`app.RegisterSites(papa.Sites()...)`。
var RegisterSite = app.RegisterSite

// CheckSites 校验一批站点与阶段声明，**不连库、不建池、不改任何状态**：把 RegisterSites 会在启动时
// panic 的那些问题**一次收集全**返回（nil = 都没问题）。给"声明写完了先自查一遍"用（`papa check`）。
//
// cfg 提供全局默认（现在是 crawler.breaker 的默认阈值）；传 nil = 没有全局默认，只用声明里写的值。
var CheckSites = app.CheckSites

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
